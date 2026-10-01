package session

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// Cleanup is limited to directories of calls recorded without a definite
// answer. Another session's reserved files are never opened or removed.
func (w *workspace) cleanupWrites(records []record) error {
	done := map[string]bool{}
	for _, r := range records {
		if r.Type == recordToolResult && r.Outcome != outcomeUnknown {
			done[r.key()] = true
		}
	}
	prefix := ".harness-workbench-" + strings.ReplaceAll(w.id, "-", "") + "-"
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
			c, dir, _, code := w.writeParent(context.Background(), rel, false)
			if code != "" {
				continue
			}
			f, err := dir.Open(".")
			if err != nil {
				c.close()
				return stateError(StateUnusable)
			}
			for {
				entries, e := f.ReadDir(workbenchDirBatch)
				for _, entry := range entries {
					name := entry.Name()
					if strings.HasPrefix(name, prefix) && wsfile.Reserved(name) && !entry.IsDir() {
						if e := dir.Remove(path.Base(name)); e != nil && !errors.Is(e, fs.ErrNotExist) {
							f.Close()
							c.close()
							return stateError(StateUnusable)
						}
					}
				}
				if e != nil {
					break
				}
			}
			f.Close()
			c.close()
		}
	}
	return nil
}
