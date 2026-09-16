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
)

func TestCodexFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		text string
		kind ErrorKind
	}{
		{"Selected model is at capacity. Please try a different model.", ErrorOverloaded},
		{"unexpected status 503 Service Unavailable: secret-body, url: secret", ErrorUnavailable},
		{"unexpected status 429 Too Many Requests: secret", ErrorRateLimited},
		{"exceeded retry limit, last status: 503 Service Unavailable, request id: secret", ErrorUnavailable},
		{"unexpected status 401 Unauthorized: secret", ErrorAuthentication},
		{"unexpected status 402 Payment Required: secret", ErrorUnknown},
		{"request timed out secret", ErrorUnknown},
		{"agent says unexpected status 503 Service Unavailable: secret", ErrorUnknown},
	} {
		t.Run(string(tc.kind)+tc.text[:8], func(t *testing.T) {
			msg, _ := json.Marshal(tc.text)
			data := []byte(`{"type":"turn.failed","error":{"message":` + string(msg) + `}}`)
			_, _, err := parseCodex(data, nil)
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Kind != tc.kind {
				t.Fatalf("%v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("leaked provider data")
			}
			if failure.Retryable() != (tc.kind == ErrorOverloaded || tc.kind == ErrorUnavailable || tc.kind == ErrorRateLimited) {
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
		kind   ErrorKind
	}{{"overloaded", ErrorOverloaded}, {"rate_limit", ErrorRateLimited}, {"server_error", ErrorUnavailable}, {"authentication_failed", ErrorAuthentication}, {"billing_error", ErrorUnknown}, {"invalid_request", ErrorUnknown}} {
		data := []byte(`{"type":"assistant","error":"` + tc.native + `","message":{"content":[{"type":"text","text":"secret"}]}}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["secret"]}`)
		_, _, err := parseClaude(data, nil)
		var failure *RequestError
		if !errors.As(err, &failure) || failure.Kind != tc.kind {
			t.Fatalf("%s: %v", tc.native, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("leaked provider data")
		}
		for _, partial := range []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}`, `{"type":"result","subtype":"success","is_error":false}`, `{"type":"stream_event"}`, `bad-json`} {
			if claudeRequestFailure(append([]byte(partial+"\n"), data...)) != nil {
				t.Fatalf("partial accepted: %s", partial)
			}
		}
	}
	if claudeRequestFailure([]byte(`{"type":"assistant","error":"rate_limit"}`)) != nil {
		t.Fatal("missing terminal result")
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
		if failure := claudeRequestFailure(data); failure == nil || !failure.Retryable() {
			t.Fatal("terminal CLI failure was lost")
		}
	}
}

func TestClaudeTerminalDiagnosticsStaySafeAndNonretryable(t *testing.T) {
	// These enums are from the installed Claude 2.1.273 stream-json schema. The
	// prose, payloads, unknown enum values and permission arguments are discarded.
	for _, tc := range []struct {
		name, assistant, subtype, reason, stop string
		kind                                   ErrorKind
		code                                   string
	}{
		{"structure", "", "error_max_structured_output_retries", "", "", ErrorStructuredOutputLimit, "error_max_structured_output_retries"},
		{"structure-after-overload", "overloaded", "error_max_structured_output_retries", "", "", ErrorStructuredOutputLimit, "error_max_structured_output_retries"},
		{"context", "invalid_request", "error_during_execution", "prompt_too_long", "", ErrorContextLimit, "prompt_too_long"},
		{"context-stop", "", "error_during_execution", "", "model_context_window_exceeded", ErrorContextLimit, "model_context_window_exceeded"},
		{"model", "model_not_found", "error_during_execution", "", "", ErrorModelUnavailable, "model_not_found"},
		{"organization", "oauth_org_not_allowed", "error_during_execution", "", "", ErrorPermissionDenied, "oauth_org_not_allowed"},
		{"turns", "", "error_max_turns", "", "", ErrorUnknown, "error_max_turns"},
		{"budget", "", "error_max_budget_usd", "", "", ErrorUnknown, "error_max_budget_usd"},
		{"execution", "", "error_during_execution", "", "", ErrorUnknown, "error_during_execution"},
		{"partial-overload", "overloaded", "error_during_execution", "", "", ErrorUnknown, "overloaded"},
		{"unknown-values", "secret-credential", "secret-subtype", "secret-reason", "secret-stop", ErrorUnknown, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := json.Marshal(map[string]any{"type": "result", "is_error": true, "subtype": tc.subtype, "terminal_reason": tc.reason, "stop_reason": tc.stop, "errors": []string{"secret-error"}, "permission_denials": []any{map[string]any{"tool_name": "secret-tool", "tool_input": "secret-data"}}})
			data := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"partial secret\"}]}}\n")
			if tc.assistant != "" {
				b, _ := json.Marshal(map[string]any{"type": "assistant", "error": tc.assistant})
				data = append(data, append(b, '\n')...)
			}
			data = append(data, result...)
			_, _, err := parseClaude(data, nil)
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Kind != tc.kind || failure.Code != tc.code || failure.Engine != "claude" || failure.Phase != PhaseResponse || failure.ExitCode != nil || failure.Retryable() {
				t.Fatalf("unexpected diagnostic: %#v (%v)", failure, err)
			}
			encoded, _ := json.Marshal(failure)
			if strings.Contains(string(encoded)+fmt.Sprintf("%#v", failure)+err.Error(), "secret") {
				t.Fatal("diagnostic retained untrusted data")
			}
			if other := claudeTerminalDiagnostic(data); other == nil || other.Kind != failure.Kind || other.Code != failure.Code || other.Retryable() {
				t.Fatalf("exit failure diagnostic differs: %#v", other)
			}
		})
	}
}

func TestProcessDiagnosticsDoNotBorrowRetryPermission(t *testing.T) {
	data := []byte(`{"type":"assistant","error":"overloaded"}` + "\n" + `{"type":"result","is_error":true,"subtype":"error_during_execution"}`)
	for _, tc := range []struct {
		err  error
		kind ErrorKind
		code string
	}{
		{context.DeadlineExceeded, ErrorTimeout, "deadline_exceeded"},
		{errOutputLimit, ErrorUnknown, "output_limit"},
		{errors.New("secret transport failure"), ErrorUnknown, ""},
	} {
		err := processRequestFailure("claude", data, tc.err)
		var failure *RequestError
		if !errors.As(err, &failure) || failure.Kind != tc.kind || failure.Code != tc.code || failure.Phase != PhaseProcess || failure.ExitCode != nil || failure.Retryable() {
			t.Fatalf("%#v", failure)
		}
		if errors.Is(err, context.DeadlineExceeded) != (tc.kind == ErrorTimeout) {
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
	_, err := restrictedCatalog([]byte(`{"models":[]}`), "secret-model-name", "high")
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Kind != ErrorModelUnavailable || failure.Code != "model_not_in_catalog" || failure.Engine != "codex" || failure.Phase != PhasePreflight || failure.Retryable() {
		t.Fatalf("%#v", failure)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("echoed arbitrary model name")
	}
}

func TestMalformedResponsesHaveSafeDiagnostics(t *testing.T) {
	for _, tc := range []struct{ engine, data, code string }{
		{"claude", `{"type":"result","subtype":"success","structured_output":{"secret":"secret"}}`, "invalid_action_envelope"},
		{"claude", "secret malformed JSON", "malformed_event_json"},
		{"claude", "", "missing_terminal_result"},
		{"codex", `{"type":"turn.completed"}`, "invalid_action_envelope"},
		{"codex", "secret malformed JSON", "malformed_event_json"},
		{"codex", "", "missing_terminal_result"},
	} {
		var err error
		if tc.engine == "claude" {
			_, _, err = parseClaude([]byte(tc.data), nil)
		} else {
			_, _, err = parseCodex([]byte(tc.data), nil)
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
		_, _, err := parseClaude(data, nil)
		var f *RequestError
		if !errors.As(err, &f) || f.Retryable() {
			t.Fatalf("%s improperly retryable: %#v", subtype, f)
		}
	}
}
