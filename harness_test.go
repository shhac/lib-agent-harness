package harness

import (
	"context"
	"errors"
	"fmt"
	"runtime"
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
	if Support(Grok, Models, ContextWindow).Availability != Native || Support(OpenAICompatible, Models, ContextWindow).Availability != Unknown || Support(Codex, Models, ContextWindow).Usable() {
		t.Fatal("context window listing claims changed")
	}
	for _, f := range []Feature{Available, Effort, Tools, CacheSplit, CostReport} {
		if c := Support(Grok, Complete, f); c.Availability != Native {
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
