package catalog

import (
	"encoding/json"
	"io"
)

const claudeRequestID = "agent-harness-models"

// readClaudeCatalog sends the SDK initialize control request, which reports
// models without a user message or inference call. Only model metadata is
// retained; account and command data in the same response never leave here.
func readClaudeCatalog(reader io.Reader, writer io.Writer) ([]Model, error) {
	if err := json.NewEncoder(writer).Encode(map[string]any{"type": "control_request", "request_id": claudeRequestID, "request": map[string]string{"subtype": "initialize"}}); err != nil {
		return nil, err
	}
	scanner := lines(reader)
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
				Subtype   string `json:"subtype"`
				Response  struct {
					Models []struct {
						Value          string   `json:"value"`
						Resolved       string   `json:"resolvedModel"`
						DisplayName    string   `json:"displayName"`
						Description    string   `json:"description"`
						SupportsEffort *bool    `json:"supportsEffort"`
						Efforts        []string `json:"supportedEffortLevels"`
						DefaultEffort  string   `json:"defaultEffortLevel"`
					} `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return nil, responseFailure("invalid_response")
		}
		if event.Type != "control_response" || event.Response.RequestID != claudeRequestID {
			continue
		}
		if event.Response.Subtype != "success" {
			return nil, responseFailure("request_failed")
		}
		if event.Response.Response.Models == nil {
			return nil, responseFailure("invalid_catalog")
		}
		catalog := newCatalogBuilder()
		for _, m := range event.Response.Response.Models {
			// "default" is Claude's own entry for whichever model it defaults to.
			model := Model{ID: m.Value, Resolved: m.Resolved, Name: m.DisplayName, Description: m.Description, DefaultEffort: m.DefaultEffort, IsDefault: m.Value == "default"}
			switch {
			case m.Efforts != nil:
				model.EffortsKnown, model.Efforts = true, make([]Effort, 0, len(m.Efforts))
				for _, effort := range m.Efforts {
					if effort != "" {
						model.Efforts = append(model.Efforts, Effort{ID: effort, Default: effort == m.DefaultEffort})
					}
				}
			case m.SupportsEffort != nil && !*m.SupportsEffort:
				model.EffortsKnown, model.Efforts = true, []Effort{}
			}
			if err := catalog.add(model); err != nil {
				return nil, err
			}
		}
		return catalog.models, nil
	}
	return nil, endOfStream(scanner)
}
