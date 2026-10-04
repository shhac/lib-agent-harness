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
		if !ok || entry.startTurn == nil || entry.interrupt == nil || (entry.steer == nil && entry.composedSteer == "") {
			t.Errorf("%s: incomplete turn lifecycle", e)
		}
		if e.Transport() != harness.CLITransport {
			if entry.dialect != nil || entry.initialize != nil || entry.normalizePolicy != nil {
				t.Errorf("%s: an API engine has CLI parts", e)
			}
			continue
		}
		switch {
		case !ok:
			t.Errorf("%s: no entry", e)
		case entry.dialect == nil || entry.normalizePolicy == nil || entry.initialize == nil || entry.event == nil || entry.args == nil:
			t.Errorf("%s: incomplete entry", e)
		case entry.resolveHome == nil && homeVariable(e) == "":
			t.Errorf("%s: no way to find its home", e)
		}
	}
}

// An engine with no sandbox support is refused by name before anything looks
// for how to sandbox it.
func TestSandboxOnAnEngineWithoutOneIsRefused(t *testing.T) {
	for _, e := range []harness.Engine{harness.Grok, harness.CommandCode} {
		if engines[e].sandbox != nil {
			continue
		}
		o := Options{Provider: harness.Provider{Engine: e}, WorkDir: t.TempDir(), Sandbox: &Sandbox{}}
		if e == harness.Grok {
			o.Policy.GrokPermission = GrokDenyWhenAsked
		}
		if err := VerifySandbox(t.Context(), o); err == nil {
			t.Errorf("%s: a sandbox was verified", e)
		}
	}
}
