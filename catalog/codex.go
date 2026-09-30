package catalog

import (
	"context"
	"encoding/json"
	"io"
)

const (
	codexPageSize  = 100
	codexPageLimit = 10
)

// readCodexCatalog follows initialize -> initialized -> model/list, waiting
// for each reply before sending the next request. Both pagination and output
// are bounded, and hidden models are left out.
func readCodexCatalog(reader io.Reader, writer io.Writer) ([]Model, error) {
	scanner := lines(reader)
	encoder := json.NewEncoder(writer)
	request := func(id int, method string, params any) error {
		return encoder.Encode(map[string]any{"id": id, "method": method, "params": params})
	}
	reply := func(id int) (json.RawMessage, error) {
		for scanner.Scan() {
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &response) != nil {
				return nil, responseFailure("invalid_response")
			}
			if response.ID != id {
				continue
			}
			if len(response.Error) != 0 && string(response.Error) != "null" {
				return nil, responseFailure("request_failed")
			}
			if len(response.Result) == 0 || string(response.Result) == "null" {
				return nil, responseFailure("invalid_catalog")
			}
			return response.Result, nil
		}
		return nil, endOfStream(scanner)
	}
	if err := request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "lib-agent-harness", "version": "1"}}); err != nil {
		return nil, err
	}
	if _, err := reply(1); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return nil, err
	}
	catalog := newCatalogBuilder()
	cursors := make(map[string]bool)
	cursor := ""
	for page := 0; page < codexPageLimit; page++ {
		params := map[string]any{"limit": codexPageSize, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := request(page+2, "model/list", params); err != nil {
			return nil, err
		}
		raw, err := reply(page + 2)
		if err != nil {
			return nil, err
		}
		next, err := addCodexPage(catalog, raw)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return catalog.models, nil
		}
		if cursors[next] {
			return nil, responseFailure("pagination_repeated")
		}
		cursors[next] = true
		cursor = next
	}
	return nil, responseFailure("page_limit")
}

func addCodexPage(catalog *catalogBuilder, raw json.RawMessage) (string, error) {
	var result struct {
		Data []struct {
			Model                  string `json:"model"`
			DisplayName            string `json:"displayName"`
			Description            string `json:"description"`
			DefaultReasoningEffort string `json:"defaultReasoningEffort"`
			Hidden                 bool   `json:"hidden"`
			IsDefault              bool   `json:"isDefault"`
			Efforts                []struct {
				ID          string `json:"reasoningEffort"`
				Description string `json:"description"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
		NextCursor string `json:"nextCursor"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Data == nil {
		return "", responseFailure("invalid_catalog")
	}
	for _, m := range result.Data {
		if m.Hidden {
			continue
		}
		model := Model{ID: m.Model, Name: m.DisplayName, Description: m.Description, DefaultEffort: m.DefaultReasoningEffort, IsDefault: m.IsDefault}
		if m.Efforts != nil {
			model.EffortsKnown, model.Efforts = true, make([]Effort, 0, len(m.Efforts))
			for _, effort := range m.Efforts {
				if effort.ID != "" {
					model.Efforts = append(model.Efforts, Effort{ID: effort.ID, Description: effort.Description, Default: effort.ID == m.DefaultReasoningEffort})
				}
			}
		}
		if err := catalog.add(model); err != nil {
			return "", err
		}
	}
	return result.NextCursor, nil
}

// addCodexWindows fills each model's context window from the catalog bundled
// in the installed binary, which the app-server's model list does not carry.
// It is the binary's statement, not the account's: the service may differ.
// A catalog that cannot be read leaves the windows unstated rather than
// failing a discovery that has already succeeded.
func (d discoverer) addCodexWindows(ctx context.Context, command invocation, models []Model) {
	bundled := invocation{bin: command.bin, args: []string{"debug", "models", "--bundled"}, env: command.env}
	var windows map[string]int64
	_ = d.run(ctx, bundled, func(reader io.Reader, _ io.Writer) error {
		windows = readCodexWindows(io.LimitReader(reader, outputLimit))
		return nil
	})
	for i := range models {
		if window := windows[models[i].ID]; window > 0 && models[i].ContextWindow == 0 {
			models[i].ContextWindow = window
		}
	}
}

func readCodexWindows(reader io.Reader) map[string]int64 {
	var catalog struct {
		Models []struct {
			Slug   string `json:"slug"`
			Window int64  `json:"context_window"`
		} `json:"models"`
	}
	if json.NewDecoder(reader).Decode(&catalog) != nil {
		return nil
	}
	windows := make(map[string]int64, len(catalog.Models))
	for _, m := range catalog.Models {
		if m.Slug != "" && m.Window > 0 {
			windows[m.Slug] = m.Window
		}
	}
	return windows
}
