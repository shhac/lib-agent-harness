package native

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Report directories go in one temporary tree rather than the user cache. It
// is made lazily, since the synthetic-harness helper re-runs this binary.
func TestMain(m *testing.M) {
	var once sync.Once
	var root string
	reportRoots = func() []string {
		once.Do(func() { root, _ = os.MkdirTemp("", "native-reports-test-") })
		return []string{root}
	}
	code := m.Run()
	if root != "" {
		_ = os.RemoveAll(root)
	}
	os.Exit(code)
}

func neverRun(t *testing.T) func(context.Context, []string, string, io.Writer, io.Writer) error {
	return func(context.Context, []string, string, io.Writer, io.Writer) error {
		t.Error("a refused configuration reached execution")
		return nil
	}
}

func refusal(t *testing.T, c Config, r Request) harness.Facts {
	t.Helper()
	c.RunCommand = neverRun(t)
	_, err := Run(context.Background(), c, r, nil)
	facts, ok := harness.ErrorFacts(err)
	if !ok {
		t.Fatalf("untyped refusal: %v", err)
	}
	if facts.Family != harness.FailureCapability || facts.Operation != harness.Run || facts.Retryable || facts.ExitCode != nil {
		t.Fatalf("refusal facts: %+v", facts)
	}
	return facts
}

func provider(e harness.Engine) harness.Provider { return harness.Provider{Engine: e} }

func TestUnusableEnginesAndProvidersAreRefused(t *testing.T) {
	for _, tc := range []struct {
		p      harness.Provider
		engine harness.Engine
		code   string
	}{
		{provider(harness.OpenAICompatible), harness.OpenAICompatible, CodeUnsupportedEngine},
		{provider("gemini"), "", CodeUnsupportedEngine},
		{provider(""), "", CodeUnsupportedEngine},
		{harness.Provider{Engine: harness.Codex, API: harness.API{BaseURL: "https://example.test"}}, harness.Codex, "api_config_for_cli_engine"},
	} {
		facts := refusal(t, Config{Provider: tc.p}, Request{Prompt: "p"})
		if facts.Code != tc.code || facts.Engine != tc.engine {
			t.Errorf("%+v: %+v", tc.p.Engine, facts)
		}
		if _, err := Version(context.Background(), Config{Provider: tc.p, RunCommand: neverRun(t)}); err == nil {
			t.Errorf("Version accepted %+v", tc.p.Engine)
		}
	}
	if _, err := NewStream(harness.OpenAICompatible, nil, StreamOptions{}); err == nil {
		t.Error("NewStream accepted an API engine")
	}
}

func TestOptionsForAnotherEngineAreRefused(t *testing.T) {
	for name, c := range map[string]Config{
		"codex sandbox on claude":   {Provider: provider(harness.Claude), Codex: CodexOptions{Sandbox: "read-only"}},
		"claude budget on grok":     {Provider: provider(harness.Grok), Claude: ClaudeOptions{MaxBudgetUSD: 1}},
		"claude empty tools":        {Provider: provider(harness.Codex), Claude: ClaudeOptions{AllowedTools: []string{}}},
		"claude permission":         {Provider: provider(harness.Codex), Claude: ClaudeOptions{PermissionMode: "auto"}},
		"grok telemetry on codex":   {Provider: provider(harness.Codex), Grok: GrokOptions{Telemetry: GrokTelemetryReduced}},
		"grok tools on claude":      {Provider: provider(harness.Claude), Grok: GrokOptions{Tools: []string{"read"}}},
		"grok sandbox on codex":     {Provider: provider(harness.Codex), Grok: GrokOptions{Sandbox: "strict"}},
		"grok permission on claude": {Provider: provider(harness.Claude), Grok: GrokOptions{PermissionMode: "auto"}},
	} {
		if facts := refusal(t, c, Request{Prompt: "p"}); facts.Code != CodeOptionForOtherEngine || facts.Engine != c.Provider.Engine {
			t.Errorf("%s: %+v", name, facts)
		}
	}
}

func TestOptionsAnEngineCannotHonourAreRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		c    Config
		r    Request
		code string
	}{
		"negative budget":      {Config{Provider: provider(harness.Claude), Claude: ClaudeOptions{MaxBudgetUSD: -1}}, Request{}, CodeUnsupportedOption},
		"grok no tools":        {Config{Provider: provider(harness.Grok), Grok: GrokOptions{Tools: []string{}}}, Request{}, CodeUnsupportedOption},
		"grok empty tool name": {Config{Provider: provider(harness.Grok), Grok: GrokOptions{Tools: []string{""}}}, Request{}, CodeUnsupportedOption},
		"grok tool with comma": {Config{Provider: provider(harness.Grok), Grok: GrokOptions{Tools: []string{"read,write"}}}, Request{}, CodeUnsupportedOption},
		"grok bad telemetry":   {Config{Provider: provider(harness.Grok), Grok: GrokOptions{Telemetry: 7}}, Request{}, CodeUnsupportedOption},
		"invalid schema":       {Config{Provider: provider(harness.Claude)}, Request{Schema: "{"}, CodeInvalidSchema},
		"invalid codex schema": {Config{Provider: provider(harness.Codex)}, Request{Schema: "not json"}, CodeInvalidSchema},
		"codex browser":        {Config{Provider: provider(harness.Codex), Browser: true}, Request{}, CodeUnsupportedOption},
		"grok browser":         {Config{Provider: provider(harness.Grok), Browser: true}, Request{}, CodeUnsupportedOption},
	} {
		if facts := refusal(t, tc.c, tc.r); facts.Code != tc.code {
			t.Errorf("%s: %+v", name, facts)
		}
	}
}

func TestManagedFlagsInArgsAreRefused(t *testing.T) {
	refused := map[harness.Engine][][]string{
		harness.Codex: {
			{"--output-schema", "x"}, {"--output-schema=x"}, {"--output-last-message", "x"}, {"-o", "x"}, {"-ox"},
			{"--json"}, {"--sandbox", "danger-full-access"}, {"--sandbox=danger-full-access"}, {"-s", "danger-full-access"}, {"-sdanger-full-access"},
			{"--cd", "/"}, {"-C", "/"}, {"--model", "m"}, {"-m", "m"},
			{"-c", `sandbox_mode="danger-full-access"`}, {`-csandbox_mode="danger-full-access"`}, {`-c=sandbox_mode="x"`},
			{"--config", "developer_instructions='x'"}, {"--config=model_reasoning_effort=high"}, {"-c", ` "model_reasoning_effort" = "high"`}, {"-c", "model=o3"},
			{"--", "extra"},
		},
		harness.Claude: {
			{"--json-schema", "{}"}, {"--json-schema={}"}, {"--output-format", "text"}, {"--permission-mode", "bypassPermissions"},
			{"--allowedTools", "Bash"}, {"--allowed-tools=Bash"}, {"--max-budget-usd", "9"}, {"--resume", "s"}, {"-r", "s"},
			{"--model", "m"}, {"--effort=max"}, {"--append-system-prompt", "x"}, {"--chrome"}, {"--no-chrome"}, {"--"},
		},
		harness.Grok: {
			{"--single=x"}, {"-p", "x"}, {"--output-format=json"}, {"--json-schema={}"}, {"--tools=bash"}, {"--sandbox=off"},
			{"--permission-mode=bypassPermissions"}, {"--resume=s"}, {"-r", "s"}, {"--cwd=/"}, {"--model=m"}, {"-m", "m"},
			{"--reasoning-effort=high"}, {"--effort=high"}, {"--rules=x"},
		},
	}
	for engine, cases := range refused {
		for _, args := range cases {
			if facts := refusal(t, Config{Provider: provider(engine), Args: args}, Request{Prompt: "p"}); facts.Code != CodeManagedFlagInArgs {
				t.Errorf("%s %q: %+v", engine, args, facts)
			}
		}
	}

	allowed := map[harness.Engine][][]string{
		harness.Codex:  {{"--add-dir", "/extra"}, {"-c", "features.web_search=true"}, {"--config=shell_environment_policy.inherit=all"}, {"--ephemeral"}, {"--color", "never"}},
		harness.Claude: {{"--add-dir", "/extra"}, {"--disallowedTools", "WebFetch"}, {"--verbose"}, {"--fallback-model", "sonnet"}},
		harness.Grok:   {{"--no-subagents"}, {"-s", "00000000-0000-0000-0000-000000000000"}, {"--max-turns=3"}, {"--disable-web-search"}},
	}
	for engine, cases := range allowed {
		for _, args := range cases {
			if code := validate(Config{Provider: provider(engine), Args: args}, Request{Prompt: "p"}); code != nil {
				t.Errorf("%s %q refused: %v", engine, args, code)
			}
		}
	}
}

func TestStreamMustMatchTheInvocation(t *testing.T) {
	for name, tc := range map[string]struct {
		engine     harness.Engine
		structured bool
		r          Request
	}{
		"engine":                       {harness.Claude, false, Request{Prompt: "p"}},
		"structured stream, no schema": {harness.Codex, true, Request{Prompt: "p"}},
		"schema, unstructured stream":  {harness.Codex, false, Request{Prompt: "p", Schema: "{}"}},
	} {
		s, _ := NewStream(tc.engine, nil, StreamOptions{Structured: tc.structured})
		_, err := Run(context.Background(), Config{Provider: provider(harness.Codex), RunCommand: neverRun(t)}, tc.r, s)
		if facts, _ := harness.ErrorFacts(err); facts.Code != CodeStreamMismatch || facts.Family != harness.FailureCapability {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRunErrorsCarryNoProviderText(t *testing.T) {
	secret := "provider-secret-diagnostic"
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		wire := map[harness.Engine]string{
			harness.Codex:  `{"type":"turn.failed","error":{"message":"` + secret + `"}}`,
			harness.Claude: `{"type":"result","is_error":true,"result":"` + secret + `"}`,
			harness.Grok:   `{"type":"error","message":"` + secret + `"}` + "\n" + `{"type":"end","stopReason":"end_turn"}`,
		}[engine]
		c := Config{Provider: provider(engine), RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
			_, _ = io.WriteString(out, wire)
			return nil
		}}
		result, err := Run(context.Background(), c, Request{Prompt: "p"}, nil)
		facts, _ := harness.ErrorFacts(err)
		if facts != (harness.Facts{Engine: engine, Operation: harness.Run, Family: harness.FailureTurn, Code: CodeTurnFailed}) {
			t.Errorf("%s facts: %+v", engine, facts)
		}
		if strings.Contains(err.Error(), secret) || !strings.Contains(result.Failure, secret) {
			t.Errorf("%s: error %q, failure %q", engine, err, result.Failure)
		}
	}
}

// Codex counts cached input inside input_tokens; Claude and Grok report it
// beside. The shared Usage.Input counts every prompt token either way, and an
// absent Codex cache figure is unknown, not zero.
func TestCacheMapsOntoTheSharedInput(t *testing.T) {
	for name, tc := range map[string]struct {
		engine harness.Engine
		wire   string
		want   harness.Usage
		fresh  int64
	}{
		"codex with cache": {harness.Codex, `{"type":"turn.completed","usage":{"input_tokens":42000,"cached_input_tokens":30000,"output_tokens":800,"reasoning_output_tokens":120}}`,
			harness.Usage{Known: true, Input: 42000, CacheRead: 30000, Output: 800, Reasoning: 120, CacheKnown: true}, 12000},
		"codex without cache": {harness.Codex, `{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":3}}`,
			harness.Usage{Known: true, Input: 20, Output: 3}, -1},
		"claude": {harness.Claude, `{"type":"result","result":"ok","usage":{"input_tokens":12000,"output_tokens":800,"cache_read_input_tokens":30000,"cache_creation_input_tokens":500}}`,
			harness.Usage{Known: true, Input: 42500, CacheRead: 30000, CacheWrite: 500, Output: 800, CacheKnown: true}, 12000},
		"grok": {harness.Grok, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":3,"reasoning_tokens":1}}`,
			harness.Usage{Known: true, Input: 17, CacheRead: 5, CacheWrite: 2, Output: 3, Reasoning: 1, CacheKnown: true}, 10},
	} {
		s, _ := NewStream(tc.engine, nil, StreamOptions{})
		s.UserPrompt("p")
		_, _ = io.WriteString(s, tc.wire+"\n")
		s.Close()
		got := s.Snapshot().Usage
		if got != tc.want {
			t.Errorf("%s: %+v want %+v", name, got, tc.want)
		}
		fresh, ok := got.Fresh()
		if (tc.fresh < 0) == ok || (ok && fresh != tc.fresh) {
			t.Errorf("%s: fresh %d %v", name, fresh, ok)
		}
	}
}

func TestVersionTakesTheProvider(t *testing.T) {
	var gotArgs []string
	c := Config{Provider: provider(harness.Claude), RunCommand: func(_ context.Context, args []string, _ string, out, _ io.Writer) error {
		gotArgs = args
		_, _ = io.WriteString(out, "2.1.0 (Claude Code)\n")
		return nil
	}}
	version, err := Version(context.Background(), c)
	if err != nil || version != "2.1.0 (Claude Code)" || strings.Join(gotArgs, " ") != "--version" {
		t.Fatalf("%q %v %q", version, err, gotArgs)
	}
	c.Provider.CLI.Binary = filepath.Join(t.TempDir(), "missing")
	c.RunCommand = nil
	if _, err := Version(context.Background(), c); err == nil {
		t.Fatal("missing binary reported a version")
	} else if facts, _ := harness.ErrorFacts(err); facts.Code != CodeExecutableNotFound {
		t.Fatalf("%+v", facts)
	}
}

// The browser is only ever switched on by Config.Browser.
func TestClaudeBrowserFlag(t *testing.T) {
	r := Request{Prompt: "p"}
	if slices.Contains(claudeArgs(Config{Provider: provider(harness.Claude)}, r), "--chrome") {
		t.Fatal("a run without Browser enables the browser")
	}
	if !slices.Contains(claudeArgs(Config{Provider: provider(harness.Claude), Browser: true}, r), "--chrome") {
		t.Fatal("Browser does not enable the browser")
	}
}
