//go:build !windows

package session

import (
	"errors"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// The facts a caller records come from the error itself. Reading them out of a
// message would make this library's prose part of its contract, and falling back
// to "unknown" for anything unrecognized would throw away the classification
// this library works to produce.
func TestTypedFailuresPublishTheirFacts(t *testing.T) {
	status := 3
	for _, tc := range []struct {
		name string
		err  error
		want harness.Facts
	}{
		{"turn", &TurnError{Engine: harness.Claude, Code: "authentication_failed"},
			harness.Facts{Engine: harness.Claude, Family: harness.FailureTurn, Code: "authentication_failed"}},
		{"process", &ProcessError{Engine: harness.Codex, Code: ProcessExited, ExitCode: &status},
			harness.Facts{Engine: harness.Codex, Family: harness.FailureProcess, Code: ProcessExited, ExitCode: &status}},
		{"capability", &CapabilityError{Engine: harness.Codex, Code: CapabilityNativeToolsPresent, Phase: BeforeLaunch},
			harness.Facts{Engine: harness.Codex, Family: harness.FailureCapability, Code: CapabilityNativeToolsPresent, Phase: BeforeLaunch}},
		{"unsupported", &UnsupportedError{Engine: harness.Claude, Operation: "steer", Code: RefusedNotNative},
			harness.Facts{Engine: harness.Claude, Family: harness.FailureCapability, Code: RefusedNotNative}},
		{"preflight", &UnsupportedError{Engine: harness.Codex, Operation: "env", Code: RefusedEnvMalformed},
			harness.Facts{Engine: harness.Codex, Family: harness.FailurePreflight, Code: RefusedEnvMalformed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped, because that is how a caller receives it.
			got, ok := harness.ErrorFacts(errors.Join(errors.New("context"), tc.err))
			if !ok {
				t.Fatal("a typed failure carried no facts")
			}
			if got.Engine != tc.want.Engine || got.Family != tc.want.Family || got.Code != tc.want.Code || got.Phase != tc.want.Phase || got.Operation != harness.Session {
				t.Fatalf("facts disagree: %+v", got)
			}
			if (got.ExitCode == nil) != (tc.want.ExitCode == nil) {
				t.Fatalf("exit status lost: %+v", got)
			}
			if got.Retryable {
				t.Error("a failure that may have executed tools was reported as safe to repeat")
			}
		})
	}
	if _, ok := harness.ErrorFacts(errors.New("something else")); ok {
		t.Error("an unclassified error was reported as carrying facts")
	}
}

// Every refusal of a caller's options is typed, so a caller can classify it
// without reading its message.
func TestOptionRefusalsCarryFacts(t *testing.T) {
	home := t.TempDir()
	base := func(e harness.Engine) Options {
		return Options{Provider: harness.Provider{Engine: e, CLI: harness.CLI{Home: home}}, WorkDir: t.TempDir()}
	}
	with := func(o Options, edit func(*Options)) Options { edit(&o); return o }
	for _, tc := range []struct {
		name   string
		o      Options
		code   string
		family harness.Family
	}{
		{"restricted grok", with(base(harness.Grok), func(o *Options) { o.Restriction = &Restriction{} }), RefusedNotOffered, harness.FailureCapability},
		{"sandboxed grok", with(base(harness.Grok), func(o *Options) { o.Sandbox = &Sandbox{} }), RefusedNotOffered, harness.FailureCapability},
		{"claude permission on grok", with(base(harness.Grok), func(o *Options) { o.Policy.ClaudePermission = "dontAsk" }), RefusedOtherEnginePolicy, harness.FailureCapability},
		{"grok permission on codex", with(base(harness.Codex), func(o *Options) { o.Policy.GrokPermission = GrokAllowWhenAsked }), RefusedOtherEnginePolicy, harness.FailureCapability},
		{"grok telemetry on claude", with(base(harness.Claude), func(o *Options) { o.Policy.GrokTelemetry = GrokTelemetryReduced }), RefusedOtherEnginePolicy, harness.FailureCapability},
		{"grok permission value", with(base(harness.Grok), func(o *Options) { o.Policy.GrokPermission = "always" }), RefusedPolicy, harness.FailurePreflight},
		{"grok managed env", with(base(harness.Grok), func(o *Options) { o.Env = []string{"GROK_MEMORY=1"} }), RefusedEnvManaged, harness.FailureCapability},
		{"api engine with a cli home", base(harness.OpenAICompatible), "cli_config_for_api_engine", harness.FailurePreflight},
		{"unknown engine", base("other"), RefusedEngine, harness.FailureCapability},
		{"api half on a cli engine", with(base(harness.Codex), func(o *Options) { o.Provider.API.BaseURL = "https://example.test" }), "api_config_for_cli_engine", harness.FailurePreflight},
		{"instruction mode", with(base(harness.Claude), func(o *Options) { o.Instructions = Instructions{Mode: "merge"} }), RefusedInstructionMode, harness.FailurePreflight},
		{"instructions without mode", with(base(harness.Claude), func(o *Options) { o.Instructions = Instructions{Text: "x"} }), RefusedInstructionModeMissing, harness.FailurePreflight},
		{"event buffer", with(base(harness.Claude), func(o *Options) { o.EventBuffer = 1 << 20 }), RefusedLimit, harness.FailurePreflight},
		{"text limit", with(base(harness.Claude), func(o *Options) { o.MaxTextBytes = 1 << 30 }), RefusedLimit, harness.FailurePreflight},
		{"malformed env", with(base(harness.Claude), func(o *Options) { o.Env = []string{"NOVALUE"} }), RefusedEnvMalformed, harness.FailurePreflight},
		{"managed env", with(base(harness.Claude), func(o *Options) { o.Env = []string{"PATH=/x"} }), RefusedEnvManaged, harness.FailureCapability},
		{"codex policy value", with(base(harness.Codex), func(o *Options) { o.Policy.CodexSandbox = "everything" }), RefusedPolicy, harness.FailurePreflight},
		{"claude tools on codex", with(base(harness.Codex), func(o *Options) { o.Policy.ClaudeTools = []string{} }), RefusedOtherEnginePolicy, harness.FailureCapability},
		{"claude permission on codex", with(base(harness.Codex), func(o *Options) { o.Policy.ClaudePermission = "dontAsk" }), RefusedOtherEnginePolicy, harness.FailureCapability},
		{"codex sandbox on claude", with(base(harness.Claude), func(o *Options) { o.Policy.CodexSandbox = "read-only" }), RefusedOtherEnginePolicy, harness.FailureCapability},
		{"codex approval on claude", with(base(harness.Claude), func(o *Options) { o.Policy.CodexApproval = "never" }), RefusedOtherEnginePolicy, harness.FailureCapability},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalize(tc.o)
			facts, ok := harness.ErrorFacts(err)
			if !ok {
				t.Fatalf("refusal carried no facts: %v", err)
			}
			if facts.Code != tc.code || facts.Family != tc.family || facts.Engine != tc.o.Provider.Engine || facts.Operation != harness.Session {
				t.Fatalf("facts disagree: %+v", facts)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Error("a refusal lost its ErrUnsupported identity")
			}
		})
	}
}
