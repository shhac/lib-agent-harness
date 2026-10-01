//go:build !windows

package session

import (
	"encoding/json"
	"path/filepath"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestWorkbenchWriteRefGolden(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{BaseURL: "https://gateway.invalid/v1", Dialect: harness.OpenAIChatCompletions}}, Model: "model", RuntimeHome: "/state", WorkDir: "/work", Restriction: &Restriction{Tools: ToolHost{Server: "work"}}, Workbench: &Workbench{Write: true}}
	got, _ := json.Marshal(apiReference(o, "s1"))
	const want = `{"engine":"openai-compatible","id":"s1","home":"/state","work_dir":"/work","config_hash":"115cddf1c3aaabdd46a89ce58a17b679a08821dd58de53b7d093bc09c92f83a8"}`
	if string(got) != want {
		t.Fatalf("write ref: %s", got)
	}
}

// Callers persist Refs and resume from them later. These literals pin the
// persisted form of defaulted configurations: if one moves, a stored session
// stops resuming. They were recorded before the shared harness vocabulary
// replaced Engine, Binary and Home in Options, and must survive it.
func TestPersistedRefsAreStable(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
		want string
	}{
		{"codex defaults", Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: "/h"}}, Model: "gpt-5", Effort: "high", WorkDir: "/w"},
			`{"engine":"codex","id":"s1","home":"/h","work_dir":"/w","config_hash":"036ab5ebcdadb27467c8ed93b3994e566cb3c50c0a0dfef41c5f602f670e8745"}`},
		{"claude defaults", Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Home: "/h"}}, Model: "haiku", WorkDir: "/w"},
			`{"engine":"claude","id":"s1","home":"/h","work_dir":"/w","config_hash":"814563750823203c3103008245f016ff59bcf55a8bf7d265dea1d746a292ec7c"}`},
		{"claude without tools", Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Home: "/h"}}, Model: "haiku", WorkDir: "/w", Policy: Policy{ClaudeTools: []string{}}},
			`{"engine":"claude","id":"s1","home":"/h","work_dir":"/w","config_hash":"dd5e1ea1ae9f297dfdb0201576b2c1491c76eeb1edfc13d3dc49deed3674be4a"}`},
		{"codex instructions and sandbox", Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: "/h"}}, Model: "gpt-5", WorkDir: "/w", Instructions: Instructions{Mode: Append, Text: "be brief"}, Policy: Policy{CodexSandbox: "workspace-write"}},
			`{"engine":"codex","id":"s1","home":"/h","work_dir":"/w","config_hash":"b5f413771e4a1d0746201df017a752d558032a9eda6c68cb82911d08ce7bfa72"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := normalize(tc.o)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(reference(o, "s1"))
			if string(got) != tc.want {
				t.Fatalf("persisted ref changed:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// An empty home resolves the same way it always has, because the resolved
// path is part of every stored Ref.
func TestEmptyHomeResolution(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	for _, tc := range []struct {
		engine   harness.Engine
		key, env string
		want     string
	}{
		{harness.Codex, "CODEX_HOME", "", filepath.Join(user, ".codex")},
		{harness.Codex, "CODEX_HOME", "/elsewhere/codex", "/elsewhere/codex"},
		{harness.Claude, "CLAUDE_CONFIG_DIR", "", filepath.Join(user, ".claude")},
		{harness.Claude, "CLAUDE_CONFIG_DIR", "/elsewhere/claude", "/elsewhere/claude"},
	} {
		t.Setenv(tc.key, tc.env)
		o, err := normalize(Options{Provider: harness.Provider{Engine: tc.engine}, WorkDir: "/w"})
		if err != nil {
			t.Fatal(err)
		}
		if o.Provider.CLI.Home != tc.want || o.Provider.CLI.Binary != string(tc.engine) {
			t.Fatalf("%s with %s=%q: home %q binary %q", tc.engine, tc.key, tc.env, o.Provider.CLI.Home, o.Provider.CLI.Binary)
		}
	}
}
