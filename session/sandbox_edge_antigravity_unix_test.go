//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Preparation writes agy's files with writeBeneath, which Windows lacks: the
// edge is not offered there.
func TestAntigravityEdgeSettingsReplacePriorState(t *testing.T) {
	home := t.TempDir()
	for path, data := range map[string]string{antigravitySettings: `{"modelProvider":"other","permissions":{"allow":["command(*)"]}}`, antigravityMCPConfig: `{"mcpServers":{"planted":{}}}`, antigravityProject: `{"id":"default-cli-project","projectResources":{"planted":{}}}`} {
		path = filepath.Join(home, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	o := antigravityEdgeOptions(t)
	o.RuntimeHome = home
	if err := antigravityEdge.prepare(t.Context(), o, nil); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(home, antigravitySettings))
	if err != nil || string(settings) != "{\n  \"modelProvider\": \"gemini\"\n}\n" {
		t.Fatalf("settings not replaced in agy's own form: %q %v", settings, err)
	}
	mcp, err := os.ReadFile(filepath.Join(home, antigravityMCPConfig))
	if err != nil || len(mcp) != 0 {
		t.Fatal("planted MCP servers kept", err)
	}
	if project, err := os.ReadFile(filepath.Join(home, antigravityProject)); err != nil || !strings.Contains(string(project), `"projectResources":  {}`) {
		t.Fatal("default project not reset", string(project), err)
	}
}
