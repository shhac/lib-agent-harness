//go:build !windows

package session

import (
	harness "github.com/shhac/lib-agent-harness"
	"os"
	"path/filepath"
	"testing"
)

func TestEdgeNeverExportsForgedRuntimeLogin(t *testing.T) {
	source, runtime := t.TempDir(), t.TempDir()
	owner := []byte(`{"tokens":{"account_id":"owner","access_token":"synthetic-owner"}}`)
	if err := os.WriteFile(filepath.Join(source, codexCredentialFile), owner, 0600); err != nil {
		t.Fatal(err)
	}
	o := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: source}}, RuntimeHome: runtime, Sandbox: &Sandbox{Edge: &Edge{}}}
	h := sessionCodexRuntime(o)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	// Simulate an untrusted runtime modified by a tool or a prior draft. This
	// validates reconciliation independently of OS enforcement availability.
	if err := os.WriteFile(filepath.Join(runtime, codexCredentialFile), []byte(`{"tokens":{"account_id":"attacker","access_token":"forged"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Session{options: o}
	s.syncLogin()
	if err := s.writeBackLogin(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(source, codexCredentialFile))
	if err != nil || string(got) != string(owner) {
		t.Fatal("owner credential changed", err)
	}
	if err = h.Prepare(); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(runtime, codexCredentialFile))
	if err != nil || string(got) != string(owner) {
		t.Fatal("resume retained forged login", err)
	}
}
