package session

import (
	harness "github.com/shhac/lib-agent-harness"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plan construction does not need nested Seatbelt or credentials.
func TestClaudeEdgePrivateEnvironmentAndPlan(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	o := Options{WorkDir: t.TempDir(), RuntimeHome: home, Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: "/usr/bin/true", Home: filepath.Join(t.TempDir(), "native")}}, Sandbox: &Sandbox{Edge: &Edge{ClaudeToken: "dummy-private-token"}}}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := edgePlan(o, environmentEdgeProxy{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range [][]string{edgeEnvironment(o, environmentEdgeProxy{}), plan.Env} {
		values := map[string]string{}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
			if value == o.Provider.CLI.Home {
				t.Fatal("native home inherited")
			}
		}
		if values["HOME"] != resolved || values["CLAUDE_CONFIG_DIR"] != filepath.Join(resolved, "claude") {
			t.Fatal("private config not selected")
		}
		if values["CLAUDE_CODE_OAUTH_TOKEN"] != o.Sandbox.Edge.ClaudeToken {
			t.Fatal("token not selected")
		}
	}
}
