package harness

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestEngineSpellingsArePersistedAndStable(t *testing.T) {
	want := map[Engine]Transport{"codex": CLITransport, "claude": CLITransport, "grok": CLITransport, "openai-compatible": APITransport}
	if len(Engines()) != len(want) {
		t.Fatalf("engines %v", Engines())
	}
	for _, e := range Engines() {
		if want[e] == "" || e.Transport() != want[e] {
			t.Fatalf("%s: transport %q", e, e.Transport())
		}
	}
	if Engine("gemini").Transport() != "" {
		t.Fatal("an unknown engine has a transport")
	}
}

func TestProviderRefusesTheOtherTransportsHalf(t *testing.T) {
	token := func(context.Context) (string, error) { return "t", nil }
	api := API{BaseURL: "https://gateway.invalid/v1", Dialect: OpenAIChatCompletions, Credentials: token}
	for _, tc := range []struct {
		name string
		p    Provider
		code string
	}{
		{"cli", Provider{Engine: Codex, CLI: CLI{Binary: "codex"}}, ""},
		{"cli with api", Provider{Engine: Claude, API: api}, "api_config_for_cli_engine"},
		{"api", Provider{Engine: OpenAICompatible, API: api}, ""},
		{"api with cli", Provider{Engine: OpenAICompatible, API: api, CLI: CLI{Home: "/h"}}, "cli_config_for_api_engine"},
		{"unknown", Provider{Engine: "gemini"}, "unsupported_engine"},
		{"no dialect", Provider{Engine: OpenAICompatible, API: API{BaseURL: api.BaseURL, Credentials: token}}, "api_dialect_required"},
		{"remote plaintext", Provider{Engine: OpenAICompatible, API: API{BaseURL: "http://gateway.invalid", Dialect: OpenAIChatCompletions, Credentials: token}}, "api_base_url_insecure"},
		{"no credentials", Provider{Engine: OpenAICompatible, API: API{BaseURL: api.BaseURL, Dialect: OpenAIChatCompletions}}, "api_credentials_required"},
		{"unauthenticated remote", Provider{Engine: OpenAICompatible, API: API{BaseURL: api.BaseURL, Dialect: OpenAIChatCompletions, Unauthenticated: true}}, "api_unauthenticated_remote"},
		{"unauthenticated loopback", Provider{Engine: OpenAICompatible, API: API{BaseURL: "http://127.0.0.1:11434/v1", Dialect: OpenAIChatCompletions, Unauthenticated: true}}, ""},
		{"both credentials", Provider{Engine: OpenAICompatible, API: API{BaseURL: "http://localhost/v1", Dialect: OpenAIChatCompletions, Credentials: token, Unauthenticated: true}}, "api_credentials_conflict"},
		{"unknown effort parameter", Provider{Engine: OpenAICompatible, API: API{BaseURL: api.BaseURL, Dialect: OpenAIChatCompletions, Credentials: token, EffortParameter: "x"}}, "api_effort_parameter_unsupported"},
		{"cli with openrouter routing", Provider{Engine: Codex, API: API{OpenRouter: &OpenRouterRouting{}}}, "api_config_for_cli_engine"},
	} {
		if got := tc.p.Problem(); got != tc.code {
			t.Errorf("%s: problem %q want %q", tc.name, got, tc.code)
		}
	}
	for collection, code := range map[string]string{"": "", "allow": "", "deny": "", "maybe": "api_openrouter_data_collection_invalid", "DENY": "api_openrouter_data_collection_invalid"} {
		routed := api
		routed.OpenRouter = &OpenRouterRouting{RequireParameters: true, DataCollection: collection}
		if got := (Provider{Engine: OpenAICompatible, API: routed}).Problem(); got != code {
			t.Errorf("data collection %q: problem %q want %q", collection, got, code)
		}
	}
	for _, tc := range []struct {
		name string
		p    Provider
		code string
	}{
		{"zero routing", Provider{Engine: OpenAICompatible, API: API{BaseURL: api.BaseURL, Dialect: OpenAIChatCompletions, Credentials: token, OpenRouter: &OpenRouterRouting{}}}, ""},
	} {
		if got := tc.p.Problem(); got != tc.code {
			t.Errorf("%s: problem %q want %q", tc.name, got, tc.code)
		}
	}
}

func TestAPIEffortAndEndpoint(t *testing.T) {
	a := API{BaseURL: "https://gateway.invalid/v1/", EffortParameter: EffortReasoningObject}
	if got, code := a.Endpoint("chat", "completions"); got != "https://gateway.invalid/v1/chat/completions" || code != "" {
		t.Fatalf("%s %s", got, code)
	}
	for effort, code := range map[string]string{"": "", "high": "", "x-high_2": "api_effort_invalid", `high","model":"x`: "api_effort_invalid", "abcdefghijklmnopqrstuvwxyzabcdefg": "api_effort_invalid"} {
		if got := a.EffortProblem(effort); got != code {
			t.Errorf("%q: %q want %q", effort, got, code)
		}
	}
	if (API{}).EffortProblem("low") != "api_effort_parameter_required" {
		t.Fatal("effort without a parameter accepted")
	}
}

func TestUsageFreshNeedsACacheSplit(t *testing.T) {
	if _, ok := (Usage{Known: true, Input: 10}).Fresh(); ok {
		t.Fatal("fresh input derived without a cache split")
	}
	if fresh, ok := (Usage{Known: true, CacheKnown: true, Input: 10, CacheRead: 3, CacheWrite: 2}).Fresh(); !ok || fresh != 5 {
		t.Fatalf("fresh %d %v", fresh, ok)
	}
	if (Usage{Known: true, Input: 10, Output: 4, Reasoning: 3}).Total() != 14 {
		t.Fatal("reasoning counted twice")
	}
}

func TestSupportIsTheOnlyEngineQuestion(t *testing.T) {
	for _, e := range Engines() {
		for _, op := range Operations() {
			if Support(e, op, Available).Availability == "" {
				t.Errorf("%s %s has no availability", e, op)
			}
		}
	}
	if c := Support(OpenAICompatible, Run, StructuredOutput); c.Usable() || c.Reason == "" {
		t.Fatalf("a feature of an unsupported operation is usable: %+v", c)
	}
	if c := Support("gemini", Complete, Available); c.Usable() {
		t.Fatal("an unknown engine is usable")
	}
	if Support(Claude, Session, Compact).Usable() || !Support(Codex, Session, Compact).Usable() {
		t.Fatal("compaction claims changed")
	}
	if Support(Codex, Run, ProgressMessages).Usable() {
		t.Fatal("Codex constrains every message to the schema")
	}
	for _, e := range Engines() {
		if !Support(e, Models, Available).Usable() {
			t.Errorf("%s lists no models", e)
		}
	}
	if Support(OpenAICompatible, Models, Effort).Usable() || !Support(Grok, Models, Effort).Usable() {
		t.Fatal("effort listing claims changed")
	}
	if Support(Grok, Models, ContextWindow).Availability != Native || Support(OpenAICompatible, Models, ContextWindow).Availability != Unknown || Support(Codex, Models, ContextWindow).Availability != Unknown {
		t.Fatal("context window listing claims changed")
	}
	grokCompletion := Native
	if runtime.GOOS == "windows" {
		grokCompletion = Unsupported
	}
	for _, f := range []Feature{Available, Effort, Tools, CacheSplit, CostReport} {
		if c := Support(Grok, Complete, f); c.Availability != grokCompletion {
			t.Errorf("Grok completion %s: %+v", f, c)
		}
	}
	if Support(Grok, Complete, Available).Reason == "" || Support(Grok, Complete, ContextWindow).Usable() || Support(Grok, Complete, StructuredOutput).Usable() {
		t.Fatal("Grok completion claims changed")
	}
	restricted := Support(Claude, Session, RestrictTools)
	if (runtime.GOOS == "windows") == restricted.Usable() {
		t.Fatalf("restricted hosting on %s: %+v", runtime.GOOS, restricted)
	}
}

func TestGrokSessionClaims(t *testing.T) {
	for _, f := range []Feature{Available, Resume, Interrupt, AppendInstructions, ReplaceInstructions} {
		if c := Support(Grok, Session, f); c.Availability != Unknown || c.Reason == "" {
			t.Errorf("Grok session %s: %+v", f, c)
		}
	}
	if c := Support(Grok, Session, Steer); c.Availability != Composed || c.Reason == "" {
		t.Fatalf("Grok steering: %+v", c)
	}
	for _, f := range []Feature{Compact, RestrictTools, Sandbox, Tools} {
		if c := Support(Grok, Session, f); c.Usable() || c.Reason == "" {
			t.Errorf("Grok session %s: %+v", f, c)
		}
	}
	if Support(Grok, Session, CacheSplit).Availability != Native || Support(Grok, Session, ContextWindow).Availability != Unknown {
		t.Fatal("Grok session accounting claims changed")
	}
	if Support(Grok, Session, CostReport).Usable() {
		t.Fatal("a Grok session claims a cost report")
	}
}

// The library runs an OpenAI-compatible session's loop itself, so what it
// offers is composed, on every platform: there is no process to contain.
func TestOpenAICompatibleSessionClaims(t *testing.T) {
	for _, f := range []Feature{Available, Resume, Interrupt, Steer, RestrictTools, Tools, AppendInstructions, ReplaceInstructions, ProvidedSkills} {
		if c := Support(OpenAICompatible, Session, f); c.Availability != Composed || c.Reason == "" {
			t.Errorf("API session %s: %+v", f, c)
		}
	}
	if c := Support(OpenAICompatible, Session, MaxOutputTokens); c.Availability != Native {
		t.Errorf("API session reply cap: %+v", c)
	}
	for _, f := range []Feature{Compact, IncludeGlobalSkills, CostReport} {
		if c := Support(OpenAICompatible, Session, f); c.Usable() || c.Reason == "" {
			t.Errorf("API session %s: %+v", f, c)
		}
	}
	if Support(OpenAICompatible, Session, CacheSplit).Availability != Unknown || Support(OpenAICompatible, Session, ContextWindow).Availability != Unknown {
		t.Fatal("API session accounting claims changed")
	}
	if Support(OpenAICompatible, Session, Effort).Availability != Native || Support(OpenAICompatible, Account, Quota).Usable() {
		t.Fatal("API session effort or quota claims changed")
	}
	// Every CLI session passes the caller's effort to its harness; a matrix
	// that said otherwise would hide a working option from callers that ask.
	for _, engine := range []Engine{Codex, Claude, Grok} {
		if c := Support(engine, Session, Effort); c.Availability != Native {
			t.Fatalf("%s session effort: %+v", engine, c)
		}
	}
}

// Every session engine reports what its tools were asked and what they
// produced: natively where the harness states it, composed where the library
// runs the loop. Nothing else claims it.
func TestToolActivityClaims(t *testing.T) {
	for _, e := range []Engine{Codex, Claude, Grok} {
		if c := Support(e, Session, ToolActivity); c.Availability != Native || c.Reason == "" {
			t.Errorf("%s session tool activity: %+v", e, c)
		}
	}
	if c := Support(OpenAICompatible, Session, ToolActivity); c.Availability != Composed || c.Reason == "" {
		t.Errorf("API session tool activity: %+v", c)
	}
	for _, op := range []Operation{Complete, Run, Models, Account} {
		for _, e := range Engines() {
			if Support(e, op, ToolActivity).Usable() {
				t.Errorf("%s %s claims tool activity", e, op)
			}
		}
	}
}

type carrier struct{ facts Facts }

func (c carrier) Error() string       { return "failed" }
func (c carrier) HarnessFacts() Facts { return c.facts }

func TestErrorFactsUnwraps(t *testing.T) {
	err := fmt.Errorf("context: %w", carrier{Facts{Engine: Codex, Family: FailureRequest, Cause: CauseRateLimited, Retryable: true}})
	facts, ok := ErrorFacts(err)
	if !ok || facts.Cause != CauseRateLimited || !facts.Retryable {
		t.Fatalf("%+v %v", facts, ok)
	}
	if _, ok := ErrorFacts(errors.New("plain")); ok {
		t.Fatal("an unclassified error has facts")
	}
}

// Every CLI engine accepts caller-provided skills in every operation that has
// an agent or a model, natively or composed; the modes implement exactly this.
func TestProvidedSkillsAreOfferedWhereverThereIsAnAgent(t *testing.T) {
	for _, e := range []Engine{Codex, Claude, Grok} {
		for _, op := range []Operation{Complete, Run, Session} {
			if !Support(e, op, Available).Usable() {
				continue
			}
			if c := Support(e, op, ProvidedSkills); !c.Usable() || c.Reason == "" && c.Availability != Native {
				t.Errorf("%s %s skills: %+v", e, op, c)
			}
		}
	}
	if Support(OpenAICompatible, Complete, ProvidedSkills).Availability != Composed {
		t.Error("API endpoints compose skills")
	}
	if Support(Claude, Complete, IncludeGlobalSkills).Usable() {
		t.Error("constrained completion never loads installed skills")
	}
}

// Loopback networking is offered only where a proof exists: a sandboxed
// Claude session. Codex's sandbox network is all or nothing.
func TestLoopbackClaims(t *testing.T) {
	claude := Support(Claude, Session, Loopback)
	if (runtime.GOOS == "windows") == (claude.Availability == Unknown) || claude.Reason == "" {
		t.Fatalf("Claude session loopback on %s: %+v", runtime.GOOS, claude)
	}
	for _, e := range Engines() {
		for _, op := range Operations() {
			if op == Session && (e == Claude || (e == OpenAICompatible && runtime.GOOS == "darwin")) {
				continue
			}
			if c := Support(e, op, Loopback); c.Usable() {
				t.Errorf("%s %s claims loopback: %+v", e, op, c)
			}
		}
	}
}

// Browser integration is native to Codex and Claude, with local setup required.
func TestBrowserClaims(t *testing.T) {
	for _, op := range []Operation{Session, Run} {
		for _, e := range []Engine{Codex, Claude} {
			if c := Support(e, op, Browser); c.Availability != Native || c.Reason == "" {
				t.Errorf("%s %s browser: %+v", e, op, c)
			}
		}
		for _, e := range []Engine{Grok} {
			if c := Support(e, op, Browser); c.Usable() || c.Reason == "" {
				t.Errorf("%s %s browser: %+v", e, op, c)
			}
		}
	}
	for _, e := range Engines() {
		for _, op := range []Operation{Complete, Models, Account} {
			if Support(e, op, Browser).Usable() {
				t.Errorf("%s %s claims a browser", e, op)
			}
		}
	}
	if Support(OpenAICompatible, Session, Browser).Usable() {
		t.Error("an API session claims a browser")
	}
}

func TestBackgroundAndToolImageClaims(t *testing.T) {
	for _, e := range []Engine{Claude, Codex, Grok} {
		for _, op := range []Operation{Session, Run} {
			if c := Support(e, op, Background); (runtime.GOOS == "windows") == c.Usable() || c.Reason == "" {
				t.Errorf("%s %s background on %s: %+v", e, op, runtime.GOOS, c)
			}
		}
	}
	if (Support(OpenAICompatible, Session, Background).Usable() != (runtime.GOOS == "darwin")) || Support(Claude, Complete, Background).Usable() {
		t.Error("background claimed where nothing is launched for it")
	}
	if Support(Claude, Session, ToolImages).Availability != Native || Support(Codex, Session, ToolImages).Availability != Native || Support(Grok, Session, ToolImages).Availability != Unknown {
		t.Error("tool image claims changed")
	}
}

func TestWorkbenchCapabilityClaims(t *testing.T) {
	for _, feature := range []Feature{Sandbox, Loopback, Background} {
		want := Unsupported
		if runtime.GOOS == "darwin" {
			want = Unknown
			if feature == Background {
				want = Composed
			}
		}
		if got := Support(OpenAICompatible, Session, feature); got.Availability != want || got.Reason == "" {
			t.Fatalf("%s: %+v", feature, got)
		}
	}
	reason := "the library's read_file, list_files and search_files: regular, singly linked files reached inside WorkDir on WorkDir's own mount"
	read := Support(OpenAICompatible, Session, WorkspaceRead)
	supported := runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if supported && (read.Availability != Composed || read.Reason != reason) || !supported && read.Usable() {
		t.Fatal(read)
	}
	if write := Support(OpenAICompatible, Session, WorkspaceWrite); write.Availability != Composed {
		t.Fatal(write)
	}
	if c := Support(OpenAICompatible, Session, RestrictTools); c.Reason != "only the caller's hosted tools and the library's workbench tools exist: the library writes every request itself" {
		t.Fatal(c)
	}
	for _, e := range []Engine{Codex, Claude, Grok} {
		for _, f := range []Feature{WorkspaceRead, WorkspaceWrite} {
			if Support(e, Session, f).Usable() {
				t.Fatalf("%s %s", e, f)
			}
		}
	}
}

// Which engines admit the browser in a sandboxed session, as the session
// package refuses it: Claude and Codex do, and an engine without a
// browser doesn't either.
func TestSandboxedBrowserClaims(t *testing.T) {
	if c := Support(Claude, Session, SandboxedBrowser); c.Availability != Native || c.Reason == "" {
		t.Fatalf("claude: %+v", c)
	}
	if c := Support(Codex, Session, SandboxedBrowser); c.Availability != Native || !strings.Contains(c.Reason, "proven before launch") || !strings.Contains(c.Reason, "no computer use") {
		t.Fatalf("codex: %+v", c)
	}
	for _, e := range []Engine{Grok, OpenAICompatible} {
		if c := Support(e, Session, SandboxedBrowser); c.Usable() || c.Reason == "" {
			t.Fatalf("%s: %+v", e, c)
		}
	}
	if Support(Claude, Run, SandboxedBrowser).Usable() {
		t.Fatal("a native run has no sandbox to admit the browser in")
	}
}
