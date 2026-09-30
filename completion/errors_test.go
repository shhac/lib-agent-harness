package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func TestCodexFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		text string
		kind harness.Cause
	}{
		{"Selected model is at capacity. Please try a different model.", harness.CauseOverloaded},
		{"unexpected status 503 Service Unavailable: secret-body, url: secret", harness.CauseUnavailable},
		{"unexpected status 429 Too Many Requests: secret", harness.CauseRateLimited},
		{"exceeded retry limit, last status: 503 Service Unavailable, request id: secret", harness.CauseUnavailable},
		{"unexpected status 401 Unauthorized: secret", harness.CauseAuthentication},
		{"unexpected status 402 Payment Required: secret", harness.CauseUnknown},
		{"request timed out secret", harness.CauseUnknown},
		{"agent says unexpected status 503 Service Unavailable: secret", harness.CauseUnknown},
	} {
		t.Run(string(tc.kind)+tc.text[:8], func(t *testing.T) {
			msg, _ := json.Marshal(tc.text)
			data := []byte(`{"type":"turn.failed","error":{"message":` + string(msg) + `}}`)
			_, err := parseCodex(data, nil)
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Cause != tc.kind {
				t.Fatalf("%v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("leaked provider data")
			}
			if failure.Retryable() != (tc.kind == harness.CauseOverloaded || tc.kind == harness.CauseUnavailable || tc.kind == harness.CauseRateLimited) {
				t.Fatal("retryability")
			}
			for _, prefix := range []string{`{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}`, `{"type":"turn.completed"}`, `invalid`} {
				if got := codexRequestFailure([]byte(prefix + "\n" + string(data))); got != nil {
					t.Fatalf("partial accepted: %s", prefix)
				}
			}
		})
	}
}

func TestClaudeFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		native string
		kind   harness.Cause
	}{{"overloaded", harness.CauseOverloaded}, {"rate_limit", harness.CauseRateLimited}, {"server_error", harness.CauseUnavailable}, {"authentication_failed", harness.CauseAuthentication}, {"billing_error", harness.CauseQuotaExhausted}, {"invalid_request", harness.CauseUnknown}} {
		data := []byte(`{"type":"assistant","error":"` + tc.native + `","message":{"content":[{"type":"text","text":"secret"}]}}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["secret"]}`)
		_, err := parseClaude(data, nil)
		var failure *RequestError
		if !errors.As(err, &failure) || failure.Cause != tc.kind {
			t.Fatalf("%s: %v", tc.native, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("leaked provider data")
		}
		for _, partial := range []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}`, `{"type":"result","subtype":"success","is_error":false}`, `{"type":"stream_event"}`, `bad-json`} {
			if refusalOnly(append([]byte(partial+"\n"), data...)) != nil {
				t.Fatalf("partial accepted: %s", partial)
			}
		}
	}
	if refusalOnly([]byte(`{"type":"assistant","error":"rate_limit"}`)) != nil {
		t.Fatal("missing terminal result")
	}
}

// claudeRefusal is how claude 2.1.285 reports a request the API refused: a
// synthetic assistant error, then an errored result labelled "success". A
// subscription login is told why first, in a rate_limit_event. Trimmed from
// frames captured against a local server answering 429.
func claudeRefusal(assistantError string, rateLimit string) []byte {
	lines := []string{`{"type":"system","subtype":"init","tools":["StructuredOutput"],"model":"claude-haiku-4-5-20251001","apiKeySource":"none","claude_code_version":"2.1.285"}`}
	if rateLimit != "" {
		lines = append(lines, `{"type":"rate_limit_event","rate_limit_info":`+rateLimit+`,"uuid":"u","session_id":"s"}`)
	}
	lines = append(lines,
		`{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":"You've hit your session limit · secret"}]},"parent_tool_use_id":null,"error":"`+assistantError+`","is_api_error_message":true}`,
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"duration_api_ms":0,"result":"secret","stop_reason":"stop_sequence","total_cost_usd":0,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},"terminal_reason":"api_error"}`)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestClaudeRefusalOnCurrentCLI(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second).UTC()
	rejected := fmt.Sprintf(`{"status":"rejected","resetsAt":%d,"rateLimitType":"five_hour","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":%d}}}`, reset.Unix(), reset.Unix())
	for _, tc := range []struct {
		name, error, rateLimit string
		cause                  harness.Cause
		retryable              bool
		resets                 bool
	}{
		{"api key rate limit", "rate_limit", "", harness.CauseRateLimited, true, false},
		{"overloaded", "overloaded", "", harness.CauseOverloaded, true, false},
		{"authentication", "authentication_failed", "", harness.CauseAuthentication, false, false},
		{"subscription window", "rate_limit", rejected, harness.CauseQuotaExhausted, false, true},
		{"allowed window", "rate_limit", `{"status":"allowed_warning","resetsAt":1}`, harness.CauseRateLimited, true, false},
		{"implausible reset", "rate_limit", `{"status":"rejected","resetsAt":1}`, harness.CauseQuotaExhausted, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := claudeRefusal(tc.error, tc.rateLimit)
			_, err := parseClaude(data, nil)
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Cause != tc.cause || failure.Code != tc.error {
				t.Fatalf("%+v", err)
			}
			facts, _ := harness.ErrorFacts(err)
			if facts.Retryable != tc.retryable || (facts.ResetsAt != nil) != tc.resets || (tc.resets && !facts.ResetsAt.Equal(reset)) {
				t.Fatalf("facts %+v", facts)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("leaked provider data")
			}
			// Output before the refusal withdraws retry permission, but the
			// terminal diagnostic still names what it can without it.
			partial := append([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}`+"\n"), data...)
			if refusalOnly(partial) != nil {
				t.Fatal("partial output accepted as a clean refusal")
			}
			if diagnostic := terminalFailure(partial); diagnostic == nil || diagnostic.Retryable() || diagnostic.Code != tc.error {
				t.Fatalf("diagnostic %+v", diagnostic)
			}
		})
	}
	// Only an API refusal is labelled this way; any other errored "success"
	// is not evidence of a transient rejection.
	other := strings.Replace(string(claudeRefusal("rate_limit", "")), `"terminal_reason":"api_error"`, `"terminal_reason":"max_turns"`, 1)
	if refusalOnly([]byte(other)) != nil {
		t.Fatal("non-API errored result granted retry permission")
	}
	// Only a stream that did nothing but refuse lends retry permission.
	refusal := string(claudeRefusal("rate_limit", ""))
	lines := strings.Split(strings.TrimSpace(refusal), "\n")
	for name, stream := range map[string]string{
		"output after the result": refusal + `{"type":"assistant","message":{"content":[{"type":"text","text":"more"}]}}` + "\n",
		"a second refusal":        strings.Join([]string{lines[0], lines[1], lines[1], lines[2]}, "\n"),
		"a second result":         refusal + lines[2] + "\n",
		"a compaction":            strings.Join([]string{lines[0], `{"type":"system","subtype":"compact_boundary"}`, lines[1], lines[2]}, "\n"),
	} {
		if refusalOnly([]byte(stream)) != nil {
			t.Fatalf("%s granted retry permission", name)
		}
	}
	// A later event replaces an earlier one: a window that reopened explains
	// nothing, and the refusal is an ordinary rate limit again.
	reopened := claudeRefusal("rate_limit", fmt.Sprintf(`{"status":"rejected","resetsAt":%d}`, reset.Unix()))
	reopened = []byte(strings.Replace(string(reopened), `{"type":"assistant"`, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning"}}`+"\n"+`{"type":"assistant"`, 1))
	if failure := refusalOnly(reopened); failure == nil || failure.Cause != harness.CauseRateLimited || failure.ResetsAt != nil || !failure.Retryable() {
		t.Fatalf("a reopened window still explained the refusal: %+v", failure)
	}
	// A rate_limit_event this library cannot read is not skipped silently.
	if refusalOnly(claudeRefusal("rate_limit", `{"status":"surprise"}`)) != nil {
		t.Fatal("unknown rate limit status accepted")
	}
}

func TestFailureCLIHelper(t *testing.T) {
	if os.Getenv("HARNESS_FAILURE_HELPER") != "1" {
		return
	}
	fmt.Println(`{"type":"assistant","error":"overloaded","message":{"content":[{"type":"text","text":"secret"}]}}`)
	fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true}`)
	if os.Getenv("HARNESS_FAILURE_WAIT") == "1" {
		time.Sleep(time.Minute)
	}
	os.Exit(1)
}
func TestFailedCLIClassificationAndTimeout(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range []bool{false, true} {
		env := append(os.Environ(), "HARNESS_FAILURE_HELPER=1")
		if wait {
			env = append(env, "HARNESS_FAILURE_WAIT=1")
		}
		data, runErr := runCLI(context.Background(), Config{Timeout: 300 * time.Millisecond}, bin, []string{"-test.run=^TestFailureCLIHelper$"}, t.TempDir(), env, "")
		if wait {
			if !errors.Is(runErr, context.DeadlineExceeded) {
				t.Fatalf("%v", runErr)
			}
			continue
		}
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("%v", runErr)
		}
		var failure *RequestError
		requestErr := processRequestFailure("claude", data, runErr)
		if !errors.As(requestErr, &failure) || failure.ExitCode == nil || *failure.ExitCode != 1 || failure.Code != "overloaded" || failure.Phase != PhaseResponse {
			t.Fatalf("lost process diagnostic: %#v", failure)
		}
		if failure := refusalOnly(data); failure == nil || !failure.Retryable() {
			t.Fatal("terminal CLI failure was lost")
		}
	}
}

func TestClaudeTerminalDiagnosticsStaySafeAndNonretryable(t *testing.T) {
	// These enums are from the installed Claude 2.1.273 stream-json schema. The
	// prose, payloads, unknown enum values and permission arguments are discarded.
	for _, tc := range []struct {
		name, assistant, subtype, reason, stop string
		kind                                   harness.Cause
		code                                   string
	}{
		{"structure", "", "error_max_structured_output_retries", "", "", harness.CauseStructuredOutputLimit, "error_max_structured_output_retries"},
		{"structure-after-overload", "overloaded", "error_max_structured_output_retries", "", "", harness.CauseStructuredOutputLimit, "error_max_structured_output_retries"},
		{"context", "invalid_request", "error_during_execution", "prompt_too_long", "", harness.CauseContextLimit, "prompt_too_long"},
		{"context-stop", "", "error_during_execution", "", "model_context_window_exceeded", harness.CauseContextLimit, "model_context_window_exceeded"},
		{"model", "model_not_found", "error_during_execution", "", "", harness.CauseModelUnavailable, "model_not_found"},
		{"organization", "oauth_org_not_allowed", "error_during_execution", "", "", harness.CausePermissionDenied, "oauth_org_not_allowed"},
		{"turns", "", "error_max_turns", "", "", harness.CauseUnknown, "error_max_turns"},
		{"budget", "", "error_max_budget_usd", "", "", harness.CauseUnknown, "error_max_budget_usd"},
		{"execution", "", "error_during_execution", "", "", harness.CauseUnknown, "error_during_execution"},
		{"partial-overload", "overloaded", "error_during_execution", "", "", harness.CauseUnknown, "overloaded"},
		{"unknown-values", "secret-credential", "secret-subtype", "secret-reason", "secret-stop", harness.CauseUnknown, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := json.Marshal(map[string]any{"type": "result", "is_error": true, "subtype": tc.subtype, "terminal_reason": tc.reason, "stop_reason": tc.stop, "errors": []string{"secret-error"}, "permission_denials": []any{map[string]any{"tool_name": "secret-tool", "tool_input": "secret-data"}}})
			data := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"partial secret\"}]}}\n")
			if tc.assistant != "" {
				b, _ := json.Marshal(map[string]any{"type": "assistant", "error": tc.assistant})
				data = append(data, append(b, '\n')...)
			}
			data = append(data, result...)
			_, err := parseClaude(data, nil)
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Cause != tc.kind || failure.Code != tc.code || failure.Engine != "claude" || failure.Phase != PhaseResponse || failure.ExitCode != nil || failure.Retryable() {
				t.Fatalf("unexpected diagnostic: %#v (%v)", failure, err)
			}
			encoded, _ := json.Marshal(failure)
			if strings.Contains(string(encoded)+fmt.Sprintf("%#v", failure)+err.Error(), "secret") {
				t.Fatal("diagnostic retained untrusted data")
			}
			if other := terminalFailure(data); other == nil || other.Cause != failure.Cause || other.Code != failure.Code || other.Retryable() {
				t.Fatalf("exit failure diagnostic differs: %#v", other)
			}
		})
	}
}

func TestProcessDiagnosticsDoNotBorrowRetryPermission(t *testing.T) {
	data := []byte(`{"type":"assistant","error":"overloaded"}` + "\n" + `{"type":"result","is_error":true,"subtype":"error_during_execution"}`)
	for _, tc := range []struct {
		err  error
		kind harness.Cause
		code string
	}{
		{context.DeadlineExceeded, harness.CauseTimeout, "deadline_exceeded"},
		{errOutputLimit, harness.CauseUnknown, "output_limit"},
		{errors.New("secret transport failure"), harness.CauseUnknown, ""},
	} {
		err := processRequestFailure("claude", data, tc.err)
		var failure *RequestError
		if !errors.As(err, &failure) || failure.Cause != tc.kind || failure.Code != tc.code || failure.Phase != PhaseProcess || failure.ExitCode != nil || failure.Retryable() {
			t.Fatalf("%#v", failure)
		}
		if errors.Is(err, context.DeadlineExceeded) != (tc.kind == harness.CauseTimeout) {
			t.Fatal("lost timeout identity")
		}
		if strings.Contains(fmt.Sprintf("%#v", failure), "secret") {
			t.Fatal("leaked transport diagnostic")
		}
	}
	if !errors.Is(processRequestFailure("claude", data, context.Canceled), context.Canceled) {
		t.Fatal("lost cancellation")
	}
}

func TestCodexPreflightModelDiagnostic(t *testing.T) {
	_, _, err := restrictedCatalog([]byte(`{"models":[]}`), "secret-model-name", "high")
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Cause != harness.CauseModelUnavailable || failure.Code != "model_not_in_catalog" || failure.Engine != "codex" || failure.Phase != PhasePreflight || failure.Retryable() {
		t.Fatalf("%#v", failure)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("echoed arbitrary model name")
	}
}

func TestMalformedResponsesHaveSafeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		engine     harness.Engine
		data, code string
	}{
		{"claude", `{"type":"result","subtype":"success","structured_output":{"secret":"secret"}}`, "invalid_action_envelope"},
		{"claude", "secret malformed JSON", "malformed_event_json"},
		{"claude", "", "missing_terminal_result"},
		{"codex", `{"type":"turn.completed"}`, "invalid_action_envelope"},
		{"codex", "secret malformed JSON", "malformed_event_json"},
		{"codex", "", "missing_terminal_result"},
	} {
		var err error
		if tc.engine == harness.Claude {
			_, err = parseClaude([]byte(tc.data), nil)
		} else {
			_, err = parseCodex([]byte(tc.data), nil)
		}
		var f *RequestError
		if !errors.As(err, &f) || f.Code != tc.code || f.Phase != PhaseResponse || f.Engine != tc.engine || f.Retryable() {
			t.Fatalf("%s: %#v", tc.engine, f)
		}
		b, _ := json.Marshal(f)
		if strings.Contains(string(b), "secret") {
			t.Fatal("leaked invalid response")
		}
	}
}

func TestClaudeTerminalLimitsNeverBorrowEarlierOverload(t *testing.T) {
	for _, subtype := range []string{"error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries", "future_subtype"} {
		data := []byte(`{"type":"assistant","error":"overloaded"}` + "\n" + `{"type":"result","is_error":true,"subtype":"` + subtype + `"}`)
		_, err := parseClaude(data, nil)
		var f *RequestError
		if !errors.As(err, &f) || f.Retryable() {
			t.Fatalf("%s improperly retryable: %#v", subtype, f)
		}
	}
}

// Every RequestError reports itself in the shared vocabulary, so a caller can
// classify a completion failure the same way as any other mode's.
func TestRequestErrorFacts(t *testing.T) {
	exitCode := 2
	for _, tc := range []struct {
		name   string
		err    *RequestError
		family harness.Family
	}{
		{"unusable engine", preflightFailure(harness.Grok, "unsupported_engine"), harness.FailureCapability},
		{"advertised native tools", &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "unexpected_native_tool_catalog"}, harness.FailureRequest},
		{"attempted native tool", &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "unexpected_native_tool_call"}, harness.FailureRequest},
		{"codex native tool event", &RequestError{Cause: harness.CauseUnknown, Engine: harness.Codex, Phase: PhaseResponse, Code: "unexpected_native_tool"}, harness.FailureRequest},
		{"probe tool surface", preflightFailure(harness.Codex, "probe_unexpected_tools"), harness.FailureCapability},
		{"probe changed model", preflightFailure(harness.Codex, "probe_changed_model"), harness.FailureCapability},
		{"probe unmapped mismatch", preflightFailure(harness.Claude, "probe_mismatch"), harness.FailureCapability},
		{"effort catalog", preflightFailure(harness.Codex, "missing_effort_catalog"), harness.FailureCapability},
		{"unsupported effort", preflightFailure(harness.Codex, "unsupported_effort"), harness.FailureCapability},
		{"dialect", preflightFailure(harness.OpenAICompatible, "api_dialect_unsupported"), harness.FailureCapability},
		{"effort parameter", preflightFailure(harness.OpenAICompatible, "api_effort_parameter_unsupported"), harness.FailureCapability},
		{"output cap", preflightFailure(harness.Codex, "max_output_tokens_unsupported"), harness.FailureCapability},
		{"probe output cap", preflightFailure(harness.Claude, "probe_changed_max_output_tokens"), harness.FailureCapability},

		{"model required", preflightFailure(harness.Codex, "model_required"), harness.FailurePreflight},
		{"probe timeout", preflightFailure(harness.Claude, "probe_timeout"), harness.FailurePreflight},
		{"missing executable", preflightFailure(harness.Claude, "executable_not_found"), harness.FailurePreflight},
		{"dialect required", preflightFailure(harness.OpenAICompatible, "api_dialect_required"), harness.FailurePreflight},
		{"credential source", &RequestError{Cause: harness.CauseAuthentication, Engine: harness.OpenAICompatible, Phase: PhasePreflight, Code: "credential_unavailable"}, harness.FailurePreflight},
		{"context bytes", &RequestError{Cause: harness.CauseContextLimit, Engine: harness.Codex, Phase: PhasePreflight, Code: "context_bytes"}, harness.FailurePreflight},

		{"process exit", &RequestError{Cause: harness.CauseUnknown, Engine: harness.Codex, Phase: PhaseProcess, ExitCode: &exitCode}, harness.FailureProcess},
		{"process timeout", &RequestError{Cause: harness.CauseTimeout, Engine: harness.Claude, Phase: PhaseProcess, Code: "deadline_exceeded"}, harness.FailureProcess},

		{"transport", &RequestError{Cause: harness.CauseUnknown, Engine: harness.OpenAICompatible, Phase: PhaseTransport, Code: "transport_failed"}, harness.FailureRequest},
		{"rate limited", &RequestError{Cause: harness.CauseRateLimited, Engine: harness.OpenAICompatible, Phase: PhaseResponse, Code: "http_429", RetryAfter: 7 * time.Second}, harness.FailureRequest},
		{"terminal failure", &RequestError{Cause: harness.CauseContextLimit, Engine: harness.Claude, Phase: PhaseResponse, Code: "prompt_too_long", ExitCode: &exitCode}, harness.FailureRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts, ok := harness.ErrorFacts(fmt.Errorf("caller context: %w", tc.err))
			want := harness.Facts{
				Engine:     tc.err.Engine,
				Operation:  harness.Complete,
				Family:     tc.family,
				Cause:      tc.err.Cause,
				Phase:      string(tc.err.Phase),
				Code:       tc.err.Code,
				ExitCode:   tc.err.ExitCode,
				RetryAfter: tc.err.RetryAfter,
				Retryable:  tc.err.Retryable(),
			}
			if !ok || facts != want {
				t.Fatalf("facts %+v, want %+v", facts, want)
			}
		})
	}
}

// Facts come from what Complete actually returned, end to end.
func TestCompleteFailuresCarryFacts(t *testing.T) {
	_, err := Complete(context.Background(), Config{Provider: harness.Provider{Engine: "gemini"}}, nil, nil)
	facts, ok := harness.ErrorFacts(err)
	if !ok || facts.Engine != "" || facts.Family != harness.FailureCapability || facts.Code != "unsupported_engine" || facts.Operation != harness.Complete || facts.Retryable {
		t.Fatalf("%+v", facts)
	}
	_, err = Complete(context.Background(), apiConfig(respondWith(429, ``, "Retry-After", "3")), userMessage, nil)
	facts, ok = harness.ErrorFacts(err)
	if !ok || facts.Family != harness.FailureRequest || facts.Cause != harness.CauseRateLimited || facts.Phase != "response" || facts.RetryAfter != 3*time.Second || !facts.Retryable {
		t.Fatalf("%+v", facts)
	}
	encoded, _ := json.Marshal(facts)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("facts retained provider text or credential")
	}
}

// refusalOnly is the failure of a stream that did nothing but refuse, the only
// kind that may lend retry permission; nil otherwise.
func refusalOnly(data []byte) *RequestError {
	failure, only := claudeFailure(data)
	if !only {
		return nil
	}
	return failure
}

// terminalFailure is the failure any errored stream reports, partial output
// or not.
func terminalFailure(data []byte) *RequestError {
	failure, _ := claudeFailure(data)
	return failure
}
