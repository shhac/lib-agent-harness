package session

import "encoding/json"

// dialect is how one engine talks over its wire: how a request is framed,
// which frames are replies and what they say, and how a request the harness
// itself sends is answered. A session resolves it once, when its wire opens.
type dialect struct {
	envelope func(id, method string, params map[string]any) map[string]any
	// parseReply reads a reply into the request it answers and its outcome.
	// isReply is false for any other frame, and a reply that cannot be read
	// is ErrProtocol.
	parseReply func(map[string]json.RawMessage) (id string, r response, isReply bool, err error)
	// answer is the reply to a server-originated request, and false for a
	// frame that is not one. Every operation the engine does not name is
	// refused: the harness never grants a permission just to unblock itself.
	answer func(map[string]json.RawMessage) (reply map[string]any, isRequest bool)
}

// dialectOf is engine's dialect under policy, and false for an engine the
// session package does not run over a process wire.
func dialectOf(o Options) (dialect, bool) {
	entry, ok := engines[o.Provider.Engine]
	if !ok {
		return dialect{}, false
	}
	return entry.dialect(o.Policy), true
}

// jsonRPCRequest reports a JSON-RPC frame that asks something of the client.
func jsonRPCRequest(m map[string]json.RawMessage) bool {
	return len(m["id"]) != 0 && len(m["method"]) != 0
}

// refusedOperation is the error every JSON-RPC engine answers an operation
// the harness does not authorize with.
func refusedOperation() map[string]any {
	return map[string]any{"code": -32601, "message": "Client does not authorize this operation"}
}
