package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// The failed turns codex-cli 0.159.0's app-server reported against a local
// server answering each refusal; the messages are replaced with a marker that
// must never reach an error.
func TestCodexFailedTurnSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name, info, code string
		cause            harness.Cause
	}{
		{"usage limit", `"usageLimitExceeded"`, "usage_limit_exceeded", harness.CauseQuotaExhausted},
		{"overloaded", `"serverOverloaded"`, "server_overloaded", harness.CauseOverloaded},
		{"refresh token reused", `"unauthorized"`, "unauthorized", harness.CauseAuthentication},
		{"rate limited", `{"responseTooManyFailedAttempts":{"httpStatusCode":429}}`, "response_too_many_failed_attempts", harness.CauseRateLimited},
		{"expired token", `{"httpConnectionFailed":{"httpStatusCode":401}}`, "http_connection_failed", harness.CauseAuthentication},
		// A gateway error may follow the upstream having acted; it is not
		// called unavailable here any more than for an API endpoint.
		{"bad gateway", `{"responseStreamDisconnected":{"httpStatusCode":502}}`, "response_stream_disconnected", harness.CauseUnknown},
		{"no status", `{"responseStreamDisconnected":{"httpStatusCode":null}}`, "response_stream_disconnected", harness.CauseUnknown},
		{"context", `"contextWindowExceeded"`, "context_window_exceeded", harness.CauseContextLimit},
		{"unrecognized", `"secretNewKind"`, "", ""},
		{"unrecognized variant", `{"secretNewKind":{"httpStatusCode":429}}`, "", ""},
		{"absent", `null`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w := fakeSession(t, harness.Codex)
			ctx := testContext(t)
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				notify(s, `{"method":"turn/started","params":{"threadId":"session-1","turn":{"id":"turn-1"}}}`)
				notify(s, `{"method":"error","params":{"threadId":"session-1","turnId":"turn-1","willRetry":false,"error":{"message":"secret","codexErrorInfo":`+tc.info+`}}}`)
				notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"failed","error":{"message":"secret","codexErrorInfo":`+tc.info+`,"additionalDetails":null}}}}`)
				return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
			}
			turn, err := s.StartTurn(ctx, Input{"go"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = turn.Wait(ctx)
			if !errors.Is(err, ErrTurnFailed) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("%v", err)
			}
			var failure *TurnError
			if tc.code == "" {
				if errors.As(err, &failure) {
					t.Fatalf("an unknown kind was named: %+v", failure)
				}
				return
			}
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Cause != tc.cause {
				t.Fatalf("%#v", err)
			}
			if facts, _ := harness.ErrorFacts(err); facts.Retryable || facts.Family != harness.FailureTurn {
				t.Fatalf("%+v", facts)
			}
		})
	}
}
