package session

import (
	"context"
	"encoding/json"
)

// Cleanup is limited to directories of calls recorded without a definite
// answer. Another session's reserved files are never opened or removed.
func (w *workbenchHost) cleanupWrites(records []record) error {
	done := map[string]bool{}
	for _, r := range records {
		if r.Type == recordToolResult && r.Outcome != outcomeUnknown {
			done[r.key()] = true
		}
	}
	for _, r := range records {
		if r.Type != recordAssistant {
			continue
		}
		for _, call := range r.Calls {
			key := record{Response: r.Response, Call: call.ID}.key()
			if done[key] || (call.Function.Name != workbenchWriteFile && call.Function.Name != workbenchEditFile) {
				continue
			}
			var in struct{ Path string }
			if json.Unmarshal([]byte(call.Function.Arguments), &in) != nil {
				continue
			}
			rel, code := resolveName(in.Path, false)
			if code != "" {
				continue
			}
			if err := w.files.RemoveReserved(context.Background(), rel, w.id); err != nil {
				return stateError(StateUnusable)
			}
		}
	}
	return nil
}
