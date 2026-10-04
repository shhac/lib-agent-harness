package session

import (
	"encoding/json"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// claudeDialect is Claude Code's stream-json control protocol. It refuses
// every control request Claude sends the client.
func claudeDialect(Policy) dialect {
	return dialect{
		envelope: func(id, method string, params map[string]any) map[string]any {
			params = cloneMap(params)
			params["subtype"] = method
			return map[string]any{"type": "control_request", "request_id": id, "request": params}
		},
		parseReply: parseClaudeReply,
		answer:     claudeAnswer,
	}
}

func claudeAnswer(m map[string]json.RawMessage) (map[string]any, bool) {
	if str(m, "type") != "control_request" {
		return nil, false
	}
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": str(m, "request_id"), "error": "Client does not authorize this operation"}}, true
}

// parseClaudeReply reads a control_response into the request it answers and
// its outcome. isReply is false for any other frame, and a reply that cannot be
// read is ErrProtocol.
func parseClaudeReply(m map[string]json.RawMessage) (id string, r response, isReply bool, err error) {
	if str(m, "type") != "control_response" {
		return "", response{}, false, nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(m["response"], &data) != nil || data == nil {
		return "", response{}, true, ErrProtocol
	}
	id = str(data, "request_id")
	if id == "" {
		return "", response{}, true, ErrProtocol
	}
	switch str(data, "subtype") {
	case "success":
		body := data["response"]
		if !rawjson.Absent(body) {
			var payload map[string]json.RawMessage
			if json.Unmarshal(body, &payload) != nil {
				return "", response{}, true, ErrProtocol
			}
		}
		return id, response{body: body}, true, nil
	case "error":
		var message string
		if json.Unmarshal(data["error"], &message) != nil {
			return "", response{}, true, ErrProtocol
		}
		if strings.HasPrefix(message, "Unsupported control request subtype:") {
			return id, response{err: ErrUnsupported}, true, nil
		}
		return id, response{err: ErrRejected}, true, nil
	default:
		return "", response{}, true, ErrProtocol
	}
}
