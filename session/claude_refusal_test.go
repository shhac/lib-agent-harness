package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// claude 2.1.285 reports a refused request as a synthetic assistant message
// carrying the error's enum, then an errored result labelled "success". A
// subscription login is told why first, in a rate_limit_event. Trimmed from
// frames captured against a local server answering 429.
const (
	claudeSyntheticRefusal = `{"type":"assistant","session_id":"session-1","parent_tool_use_id":null,"message":{"id":"m","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your session limit · secret"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"error":"%s","is_api_error_message":true}`
	claudeRefusedResult    = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"secret","session_id":"session-1","terminal_reason":"api_error","stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0}}`
)

func TestClaudeRefusalExplainsTheFailedTurn(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second).UTC()
	rejected := fmt.Sprintf(`{"type":"rate_limit_event","session_id":"session-1","rate_limit_info":{"status":"rejected","resetsAt":%d,"rateLimitType":"five_hour","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":%d}}}}`, reset.Unix(), reset.Unix())
	for name, tc := range map[string]struct {
		frames []string
		code   string
		cause  harness.Cause
		resets bool
	}{
		"subscription window": {[]string{rejected, fmt.Sprintf(claudeSyntheticRefusal, "rate_limit")}, "rate_limit", harness.CauseQuotaExhausted, true},
		"api rate limit":      {[]string{fmt.Sprintf(claudeSyntheticRefusal, "rate_limit")}, "rate_limit", harness.CauseRateLimited, false},
		"overloaded":          {[]string{fmt.Sprintf(claudeSyntheticRefusal, "overloaded")}, "overloaded", harness.CauseOverloaded, false},
		// A subagent's limit is not why the main conversation failed.
		"subagent window": {[]string{strings.Replace(rejected, `"session_id"`, `"parent_tool_use_id":"tool-1","session_id"`, 1), fmt.Sprintf(claudeSyntheticRefusal, "rate_limit")}, "rate_limit", harness.CauseRateLimited, false},
		// A response after the refusal means the CLI recovered from it.
		"recovered": {[]string{fmt.Sprintf(claudeSyntheticRefusal, "max_output_tokens"), `{"type":"assistant","session_id":"session-1","message":{"id":"m2","content":[{"type":"text","text":"done"}]}}`}, "turn_failed", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := fakeSession(t, harness.Claude)
			ctx := testContext(t)
			turn, err := s.StartTurn(ctx, Input{"start"})
			if err != nil {
				t.Fatal(err)
			}
			for _, frame := range tc.frames {
				notify(s, frame)
			}
			notify(s, claudeRefusedResult)
			result, err := turn.Wait(ctx)
			var failure *TurnError
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Cause != tc.cause {
				t.Fatalf("%#v", err)
			}
			facts, _ := harness.ErrorFacts(err)
			if (facts.ResetsAt != nil) != tc.resets || (tc.resets && !facts.ResetsAt.Equal(reset)) || facts.Retryable {
				t.Fatalf("facts %+v", facts)
			}
			if strings.Contains(result.Text, "secret") || strings.Contains(err.Error(), "secret") {
				t.Fatal("the CLI's refusal text was reported as the agent's")
			}
			if name != "recovered" && result.Observed.Known {
				t.Fatalf("a refusal's synthetic usage was counted: %+v", result.Observed)
			}
		})
	}
}
