//go:build !windows

package session

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Policy's JSON shape is part of every stored reference digest.
func TestPolicyJSONIsStable(t *testing.T) {
	full := Policy{CodexSandbox: "a", CodexApproval: "b", ClaudePermission: "c", ClaudeTools: []string{"d"}, GrokPermission: "e", GrokTelemetry: "f", CommandCodePermission: "g"}
	for name, tc := range map[string]struct {
		p    Policy
		want string
	}{
		"empty":       {Policy{}, `{}`},
		"empty tools": {Policy{ClaudeTools: []string{}}, `{}`},
		"full":        {full, `{"codex_sandbox":"a","codex_approval":"b","claude_permission":"c","claude_tools":["d"],"grok_permission":"e","grok_telemetry":"f","command_code_permission":"g"}`},
	} {
		raw, err := json.Marshal(tc.p)
		if err != nil || string(raw) != tc.want {
			t.Errorf("%s: %s %v", name, raw, err)
		}
	}
}

// What normalization makes of each engine's options: its binary, its home with
// and without the engine's own variable, and its policy defaults.
func TestNormalizedEngineFacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "GROK_HOME"} {
		t.Setenv(key, "")
	}
	policies := map[harness.Engine]Policy{harness.Grok: {GrokPermission: GrokDenyWhenAsked}}
	var got []string
	for _, e := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok, harness.CommandCode} {
		for _, variable := range []bool{false, true} {
			if variable {
				if key := homeVariable(e); key != "" {
					t.Setenv(key, filepath.Join(home, "chosen"))
				}
			}
			n, err := normalize(Options{Provider: harness.Provider{Engine: e}, WorkDir: home, Policy: policies[e]})
			if err != nil {
				t.Fatalf("%s: %v", e, err)
			}
			policy, _ := json.Marshal(n.Policy)
			rel, _ := filepath.Rel(home, n.Provider.CLI.Home)
			got = append(got, string(e)+" variable="+map[bool]string{false: "unset", true: "set"}[variable]+" binary="+n.Provider.CLI.Binary+" home="+rel+" policy="+string(policy))
		}
		if key := homeVariable(e); key != "" {
			t.Setenv(key, "")
		}
	}
	if joined := strings.Join(got, "\n"); joined != goldenFacts {
		t.Errorf("normalized facts changed:\n%s", joined)
	}
}

// What each engine's session inherits from this process and which additions
// it refuses. Inheritance and additions are deliberately different rules.
func TestEnvironmentInheritanceAndAdditions(t *testing.T) {
	keys := []string{"PLAIN_SETTING", "CODEX_HOME", "OPENAI_API_KEY", "ANTHROPIC_MODEL", "CLAUDE_CODE_USE_BEDROCK", "GROK_HOME", "GROK_AUTH_TOKEN", "GROK_TELEMETRY_ENDPOINT_URL", "GROK_THEME", "XAI_API_KEY", "CMD_ZDR", "CMD_LOCAL_ONLY", "CMD_MODEL", "COMMANDCODE_TOKEN", "NODE_OPTIONS", "GIT_DIR", "BROWSER_USE_X"}
	for _, key := range keys {
		t.Setenv(key, "v")
	}
	var got []string
	for _, e := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok, harness.CommandCode} {
		o := Options{Provider: harness.Provider{Engine: e, CLI: harness.CLI{Home: "/h"}}}
		inherited := map[string]bool{}
		for _, entry := range baseEnvironment(o) {
			key, _, _ := strings.Cut(entry, "=")
			inherited[key] = true
		}
		var kept, refused []string
		for _, key := range keys {
			if inherited[key] {
				kept = append(kept, key)
			}
			if validateEnv(Options{Provider: o.Provider, Env: []string{key + "=v"}}) != nil {
				refused = append(refused, key)
			}
		}
		sort.Strings(kept)
		sort.Strings(refused)
		got = append(got, string(e)+" inherits "+strings.Join(kept, ",")+" refuses "+strings.Join(refused, ","))
	}
	if joined := strings.Join(got, "\n"); joined != goldenEnvironment {
		t.Errorf("environment rules changed:\n%s", joined)
	}
}

const goldenFacts = `codex variable=unset binary=codex home=.codex policy={"codex_sandbox":"read-only","codex_approval":"never","claude_permission":"dontAsk"}
codex variable=set binary=codex home=chosen policy={"codex_sandbox":"read-only","codex_approval":"never","claude_permission":"dontAsk"}
claude variable=unset binary=claude home=.claude policy={"codex_sandbox":"read-only","codex_approval":"never","claude_permission":"dontAsk"}
claude variable=set binary=claude home=chosen policy={"codex_sandbox":"read-only","codex_approval":"never","claude_permission":"dontAsk"}
grok variable=unset binary=grok home=.grok policy={"grok_permission":"deny-when-asked"}
grok variable=set binary=grok home=chosen policy={"grok_permission":"deny-when-asked"}
command-code variable=unset binary=cmd home=.commandcode policy={"command_code_permission":"deny-when-asked"}
command-code variable=set binary=cmd home=.commandcode policy={"command_code_permission":"deny-when-asked"}`

// Codex and Claude inherit other engines' variables: only their own
// credentials and homes are stripped.
const goldenEnvironment = `codex inherits BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,CODEX_HOME,COMMANDCODE_TOKEN,GIT_DIR,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY
claude inherits BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,COMMANDCODE_TOKEN,GIT_DIR,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY
grok inherits BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,COMMANDCODE_TOKEN,GIT_DIR,GROK_HOME,GROK_THEME,NODE_OPTIONS,PLAIN_SETTING refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GIT_DIR,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,NODE_OPTIONS,OPENAI_API_KEY,XAI_API_KEY
command-code inherits BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_ZDR,GIT_DIR,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CMD_MODEL,CODEX_HOME,COMMANDCODE_TOKEN,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY`
