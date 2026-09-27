package catalog

import (
	"bytes"
	"encoding/json"
	"io"
)

// grokInitialize is the only request sent. session/new would also list
// models, but it runs the user's hooks and creates a session.
const grokInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}` + "\n"

// readGrokCatalog reads the model state Grok's ACP agent attaches to its
// initialize reply (result._meta.modelState). Notifications, and anything else
// that is not the reply, are skipped. Only model metadata is retained; the
// reply's host, MCP and command data never leave here.
func readGrokCatalog(reader io.Reader, writer io.Writer) ([]Model, error) {
	if _, err := io.WriteString(writer, grokInitialize); err != nil {
		return nil, err
	}
	scanner := lines(reader)
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			return nil, responseFailure("invalid_response")
		}
		if message.Method != "" || !bytes.Equal(message.ID, []byte("1")) {
			continue
		}
		if len(message.Error) != 0 && string(message.Error) != "null" {
			return nil, responseFailure("request_failed")
		}
		return grokModels(message.Result)
	}
	return nil, endOfStream(scanner)
}

type grokModel struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Meta        struct {
		TotalContextTokens      json.RawMessage `json:"totalContextTokens"`
		SupportsReasoningEffort *bool           `json:"supportsReasoningEffort"`
		ReasoningEffort         string          `json:"reasoningEffort"`
		ReasoningEfforts        []struct {
			ID          string `json:"id"`
			Value       string `json:"value"`
			Description string `json:"description"`
			Default     bool   `json:"default"`
		} `json:"reasoningEfforts"`
	} `json:"_meta"`
}

func grokModels(result json.RawMessage) ([]Model, error) {
	var reply struct {
		Meta struct {
			ModelState *struct {
				CurrentModelID  string      `json:"currentModelId"`
				AvailableModels []grokModel `json:"availableModels"`
			} `json:"modelState"`
		} `json:"_meta"`
	}
	if json.Unmarshal(result, &reply) != nil || reply.Meta.ModelState == nil || reply.Meta.ModelState.AvailableModels == nil {
		return nil, responseFailure("invalid_catalog")
	}
	state := reply.Meta.ModelState
	catalog := newCatalogBuilder()
	for _, m := range state.AvailableModels {
		model := Model{ID: m.ModelID, Name: m.Name, Description: m.Description, IsDefault: m.ModelID == state.CurrentModelID}
		model.ContextWindow = tokenCount(m.Meta.TotalContextTokens)
		grokEfforts(&model, m)
		if err := catalog.add(model); err != nil {
			return nil, err
		}
	}
	return catalog.models, nil
}

// grokEfforts records efforts only where Grok said: supportsReasoningEffort
// false means none, and true counts only with the list of levels.
func grokEfforts(model *Model, m grokModel) {
	supports := m.Meta.SupportsReasoningEffort
	switch {
	case supports == nil:
		return
	case !*supports:
		model.EffortsKnown, model.Efforts = true, []Effort{}
		return
	case m.Meta.ReasoningEfforts == nil:
		return
	}
	model.DefaultEffort = m.Meta.ReasoningEffort
	for _, level := range m.Meta.ReasoningEfforts {
		if level.Default {
			model.DefaultEffort = grokEffortID(level.Value, level.ID)
			break
		}
	}
	model.EffortsKnown, model.Efforts = true, make([]Effort, 0, len(m.Meta.ReasoningEfforts))
	for _, level := range m.Meta.ReasoningEfforts {
		if id := grokEffortID(level.Value, level.ID); id != "" {
			model.Efforts = append(model.Efforts, Effort{ID: id, Description: level.Description, Default: id == model.DefaultEffort})
		}
	}
}

// grokEffortID prefers the value Grok accepts back over its display id.
func grokEffortID(value, id string) string {
	if value != "" {
		return value
	}
	return id
}
