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
		if failure := claudeRequestFailure(data); failure == nil || !failure.Retryable() {
			t.Fatal("terminal CLI failure was lost")
		}
	}
}
