package session

import (
	"encoding/json"

	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// codexDialect is Codex app-server's JSON-RPC without the version field. Its
// approval requests are declined with the answer each one expects, so a turn
// continues without the action; everything else is refused.
func codexDialect(Policy) dialect {
	return dialect{
		envelope: func(id, method string, params map[string]any) map[string]any {
			return map[string]any{"id": id, "method": method, "params": params}
		},
		parseReply: parseCodexReply,
		answer:     codexAnswer,
	}
}

func codexAnswer(m map[string]json.RawMessage) (map[string]any, bool) {
	if !jsonRPCRequest(m) {
		return nil, false
	}
	reply := map[string]any{"id": m["id"]}
	switch str(m, "method") {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		reply["result"] = map[string]any{"decision": "decline"}
	case "item/permissions/requestApproval":
		reply["result"] = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	case "mcpServer/elicitation/request":
		reply["result"] = map[string]any{"action": "decline", "content": nil}
	default:
		reply["error"] = refusedOperation()
	}
	return reply, true
}

// parseCodexReply reads a JSON-RPC response into the request it answers and its
// outcome. isReply is false for a notification or a server request, and a reply
// that cannot be read is ErrProtocol.
func parseCodexReply(m map[string]json.RawMessage) (id string, r response, isReply bool, err error) {
	if len(m["method"]) != 0 || len(m["id"]) == 0 {
		return "", response{}, false, nil
	}
	id = str(m, "id")
	if id == "" {
		return "", response{}, true, ErrProtocol
	}
	if rawjson.Absent(m["error"]) {
		if len(m["result"]) == 0 {
			return "", response{}, true, ErrProtocol
		}
		return id, response{body: m["result"]}, true, nil
	}
	var e struct{ Code *int }
	if json.Unmarshal(m["error"], &e) != nil || e.Code == nil || len(m["result"]) != 0 {
		return "", response{}, true, ErrProtocol
	}
	if *e.Code == -32601 {
		return id, response{err: ErrUnsupported}, true, nil
	}
	return id, response{err: ErrRejected}, true, nil
}
