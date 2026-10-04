//go:build !windows

package session

import (
	"context"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Normalize a copy for cache lookup; public entry points must receive raw
// caller options, since they apply defaults and reject foreign policy fields.
func realClaudeProofKey(raw Options) (string, error) {
	normalized, err := normalize(raw)
	if err != nil {
		return "", err
	}
	return sandboxKey(normalized, &launch{extra: sandboxArgs(normalized)})
}

func TestRealClaudeProofSetupUsesRawOptions(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	raw := sandboxOptions(t, harness.Claude, binary, true)
	raw.Provider.CLI.Home = t.TempDir()
	key, err := realClaudeProofKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Policy.CodexSandbox != "" || raw.Policy.CodexApproval != "" {
		t.Fatal("setup normalized caller options in place")
	}
	if err := VerifySandbox(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if !verified.holds(key) || invocations(t, log, "status") != 1 {
		t.Fatal("setup key differs from public proof key")
	}
	verified.mu.Lock()
	_, polluted := verified.loopback[key]
	verified.mu.Unlock()
	if polluted {
		t.Fatal("base sandbox recorded loopback observations")
	}
}
