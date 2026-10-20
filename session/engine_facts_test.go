//go:build !windows

package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Policy's JSON shape is part of every stored reference digest.
func TestPolicyJSONIsStable(t *testing.T) {
	full := Policy{CodexSandbox: "a", CodexApproval: "b", ClaudePermission: "c", ClaudeTools: []string{"d"}, GrokPermission: "e", GrokTelemetry: "f", CommandCodePermission: "g", AntigravityPermission: "h", FxPermission: "i"}
	for name, tc := range map[string]struct {
		p    Policy
		want string
	}{
		"empty":       {Policy{}, `{}`},
		"empty tools": {Policy{ClaudeTools: []string{}}, `{}`},
		"full":        {full, `{"codex_sandbox":"a","codex_approval":"b","claude_permission":"c","claude_tools":["d"],"grok_permission":"e","grok_telemetry":"f","command_code_permission":"g","antigravity_permission":"h","fx_permission":"i"}`},
	} {
		raw, err := json.Marshal(tc.p)
		if err != nil || string(raw) != tc.want {
			t.Errorf("%s: %s %v", name, raw, err)
		}
	}
}

// cliSessionEngines is every engine the session registry runs as a CLI, in
// the stable display order, so a new engine joins these goldens by being
// registered.
func cliSessionEngines() []harness.Engine {
	var out []harness.Engine
	for _, e := range harness.Engines() {
		if _, ok := engines[e]; ok && e.Transport() == harness.CLITransport {
			out = append(out, e)
		}
	}
	return out
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
	for _, e := range cliSessionEngines() {
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
	keys := []string{"PLAIN_SETTING", "CODEX_HOME", "OPENAI_API_KEY", "ANTHROPIC_MODEL", "CLAUDE_CODE_USE_BEDROCK", "GROK_HOME", "GROK_AUTH_TOKEN", "GROK_TELEMETRY_ENDPOINT_URL", "GROK_THEME", "XAI_API_KEY", "CMD_ZDR", "CMD_LOCAL_ONLY", "CMD_MODEL", "COMMANDCODE_TOKEN", "NODE_OPTIONS", "GIT_DIR", "BROWSER_USE_X", "GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTIGRAVITY_HOME", "JETSKI_X", "FX_MODEL", "FX_MAX_AGENT_STEPS", "AI_GATEWAY_API_KEY"}
	for _, key := range keys {
		t.Setenv(key, "v")
	}
	var got []string
	for _, e := range cliSessionEngines() {
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
command-code variable=set binary=cmd home=.commandcode policy={"command_code_permission":"deny-when-asked"}
antigravity variable=unset binary=agy home=.gemini policy={"antigravity_permission":"request-review"}
antigravity variable=set binary=agy home=.gemini policy={"antigravity_permission":"request-review"}
fx variable=unset binary=fx home=.fx policy={"fx_permission":"deny-when-asked"}
fx variable=set binary=fx home=.fx policy={"fx_permission":"deny-when-asked"}`

// Codex and Claude inherit other engines' variables: only their own
// credentials and homes are stripped.
const goldenEnvironment = `codex inherits AI_GATEWAY_API_KEY,ANTIGRAVITY_HOME,BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,CODEX_HOME,COMMANDCODE_TOKEN,FX_MAX_AGENT_STEPS,FX_MODEL,GEMINI_API_KEY,GIT_DIR,GOOGLE_API_KEY,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,JETSKI_X,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY
claude inherits AI_GATEWAY_API_KEY,ANTIGRAVITY_HOME,BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,COMMANDCODE_TOKEN,FX_MAX_AGENT_STEPS,FX_MODEL,GEMINI_API_KEY,GIT_DIR,GOOGLE_API_KEY,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,JETSKI_X,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY
grok inherits AI_GATEWAY_API_KEY,ANTIGRAVITY_HOME,BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,COMMANDCODE_TOKEN,FX_MAX_AGENT_STEPS,FX_MODEL,GEMINI_API_KEY,GIT_DIR,GOOGLE_API_KEY,GROK_HOME,GROK_THEME,JETSKI_X,NODE_OPTIONS,PLAIN_SETTING refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GIT_DIR,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,NODE_OPTIONS,OPENAI_API_KEY,XAI_API_KEY
command-code inherits AI_GATEWAY_API_KEY,ANTIGRAVITY_HOME,BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_ZDR,FX_MAX_AGENT_STEPS,FX_MODEL,GEMINI_API_KEY,GIT_DIR,GOOGLE_API_KEY,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,JETSKI_X,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CMD_MODEL,CODEX_HOME,COMMANDCODE_TOKEN,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY
antigravity inherits AI_GATEWAY_API_KEY,BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,COMMANDCODE_TOKEN,FX_MAX_AGENT_STEPS,FX_MODEL,GIT_DIR,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,ANTIGRAVITY_HOME,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,GEMINI_API_KEY,GIT_DIR,GOOGLE_API_KEY,JETSKI_X,NODE_OPTIONS,OPENAI_API_KEY
fx inherits AI_GATEWAY_API_KEY,ANTIGRAVITY_HOME,BROWSER_USE_X,CMD_LOCAL_ONLY,CMD_MODEL,CMD_ZDR,COMMANDCODE_TOKEN,FX_MAX_AGENT_STEPS,GEMINI_API_KEY,GIT_DIR,GOOGLE_API_KEY,GROK_AUTH_TOKEN,GROK_HOME,GROK_TELEMETRY_ENDPOINT_URL,GROK_THEME,JETSKI_X,NODE_OPTIONS,PLAIN_SETTING,XAI_API_KEY refuses ANTHROPIC_MODEL,CLAUDE_CODE_USE_BEDROCK,CODEX_HOME,FX_MODEL,GIT_DIR,NODE_OPTIONS,OPENAI_API_KEY`

// Which engine refuses which Policy field, as normalisation reports it: every
// field set alone, beside the policy each engine requires, so a new field or
// engine joins the matrix without being listed here.
func TestForeignPolicyFieldsAreRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	required := map[harness.Engine]Policy{harness.Grok: {GrokPermission: GrokDenyWhenAsked}}
	fields := reflect.TypeOf(Policy{})
	var got []string
	for _, e := range cliSessionEngines() {
		for i := range fields.NumField() {
			field := fields.Field(i)
			p := required[e]
			value := reflect.ValueOf(&p).Elem().Field(i)
			switch value.Kind() {
			case reflect.String:
				if value.String() != "" {
					continue
				}
				value.SetString("x")
			case reflect.Slice:
				value.Set(reflect.ValueOf([]string{"Read"}))
			default:
				t.Fatalf("Policy.%s has a kind this test does not set", field.Name)
			}
			outcome := "accepted"
			if _, err := normalize(Options{Provider: harness.Provider{Engine: e}, WorkDir: home, Policy: p}); err != nil {
				outcome = "error"
				var refused *UnsupportedError
				if errors.As(err, &refused) {
					outcome = refused.Code
				}
			}
			got = append(got, string(e)+" "+field.Name+" "+outcome)
		}
	}
	if joined := strings.Join(got, "\n"); joined != goldenForeignPolicy {
		t.Errorf("foreign policy refusals changed:\n%s", joined)
	}
}

const goldenForeignPolicy = `codex CodexSandbox policy_invalid
codex CodexApproval policy_invalid
codex ClaudePermission policy_for_other_engine
codex ClaudeTools policy_for_other_engine
codex GrokPermission policy_for_other_engine
codex GrokTelemetry policy_for_other_engine
codex CommandCodePermission policy_for_other_engine
codex AntigravityPermission policy_for_other_engine
codex FxPermission policy_for_other_engine
claude CodexSandbox policy_for_other_engine
claude CodexApproval policy_for_other_engine
claude ClaudePermission policy_invalid
claude ClaudeTools accepted
claude GrokPermission policy_for_other_engine
claude GrokTelemetry policy_for_other_engine
claude CommandCodePermission policy_for_other_engine
claude AntigravityPermission policy_for_other_engine
claude FxPermission policy_for_other_engine
grok CodexSandbox policy_for_other_engine
grok CodexApproval policy_for_other_engine
grok ClaudePermission policy_for_other_engine
grok ClaudeTools policy_for_other_engine
grok GrokTelemetry policy_invalid
grok CommandCodePermission policy_for_other_engine
grok AntigravityPermission policy_for_other_engine
grok FxPermission policy_for_other_engine
command-code CodexSandbox policy_for_other_engine
command-code CodexApproval policy_for_other_engine
command-code ClaudePermission policy_for_other_engine
command-code ClaudeTools policy_for_other_engine
command-code GrokPermission policy_for_other_engine
command-code GrokTelemetry policy_for_other_engine
command-code CommandCodePermission policy_invalid
command-code AntigravityPermission policy_for_other_engine
command-code FxPermission policy_for_other_engine
antigravity CodexSandbox policy_for_other_engine
antigravity CodexApproval policy_for_other_engine
antigravity ClaudePermission policy_for_other_engine
antigravity ClaudeTools policy_for_other_engine
antigravity GrokPermission policy_for_other_engine
antigravity GrokTelemetry policy_for_other_engine
antigravity CommandCodePermission policy_for_other_engine
antigravity AntigravityPermission policy_invalid
antigravity FxPermission policy_for_other_engine
fx CodexSandbox policy_for_other_engine
fx CodexApproval policy_for_other_engine
fx ClaudePermission policy_for_other_engine
fx ClaudeTools policy_for_other_engine
fx GrokPermission policy_for_other_engine
fx GrokTelemetry policy_for_other_engine
fx CommandCodePermission policy_for_other_engine
fx AntigravityPermission policy_for_other_engine
fx FxPermission policy_invalid`
