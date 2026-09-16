package completion

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

// The SDK initialize control request reports models without a user message or
// inference call. Only the model metadata is retained; account and command data
// in the same response are never returned to the dashboard.
func readClaudeModelCatalog(reader io.Reader, writer io.Writer) ([]ModelOption, error) {
	const requestID = "agent-harness-models"
	if err := json.NewEncoder(writer).Encode(map[string]any{"type": "control_request", "request_id": requestID, "request": map[string]string{"subtype": "initialize"}}); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
				Subtype   string `json:"subtype"`
				Response  struct {
					Models []struct {
						Value         string   `json:"value"`
						DisplayName   string   `json:"displayName"`
						Description   string   `json:"description"`
						Efforts       []string `json:"supportedEffortLevels"`
						DefaultEffort string   `json:"defaultEffortLevel"`
					} `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return nil, errors.New("invalid Claude catalog response")
		}
		if event.Type != "control_response" || event.Response.RequestID != requestID {
			continue
		}
		if event.Response.Subtype != "success" || event.Response.Response.Models == nil {
			return nil, errors.New("Claude model discovery failed")
		}
		result := make([]ModelOption, 0, len(event.Response.Response.Models))
		seen := map[string]bool{}
		for _, model := range event.Response.Response.Models {
			if model.Value == "" || seen[model.Value] {
				continue
			}
			seen[model.Value] = true
			option := ModelOption{ID: model.Value, Name: model.DisplayName, Description: model.Description, DefaultEffort: model.DefaultEffort, IsDefault: model.Value == "default", Efforts: []ModelEffort{}}
			if option.Name == "" {
				option.Name = option.ID
			}
			for _, effort := range model.Efforts {
				if effort != "" {
					option.Efforts = append(option.Efforts, ModelEffort{ID: effort})
				}
			}
			result = append(result, option)
		}
		return result, nil
	}
	return nil, errors.New("Claude catalog response missing or exceeds limit")
}
