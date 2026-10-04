package session

import (
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Every CLI engine has a complete registry entry and nothing else has one, so
// a new engine cannot fall back on another engine's behaviour.
func TestEngineRegistryIsComplete(t *testing.T) {
	for _, e := range harness.Engines() {
		entry, ok := engines[e]
		if e.Transport() != harness.CLITransport {
			if ok {
				t.Errorf("%s: an API engine has a CLI entry", e)
			}
			continue
		}
		switch {
		case !ok:
			t.Errorf("%s: no entry", e)
		case entry.dialect == nil || entry.normalizePolicy == nil || entry.binary == "" || entry.initialize == nil || entry.event == nil:
			t.Errorf("%s: incomplete entry", e)
		case entry.resolveHome == nil && (entry.homeVariable == "" || entry.homeDir == ""):
			t.Errorf("%s: no way to find its home", e)
		}
	}
}
