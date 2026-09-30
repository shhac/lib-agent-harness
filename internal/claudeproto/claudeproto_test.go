package claudeproto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func TestOnlySchemaValuesAreRetained(t *testing.T) {
	for _, code := range []string{"authentication_failed", "rate_limit", "overloaded", "cloud_credential_error", "unknown"} {
		if ErrorCode(code) != code {
			t.Errorf("schema error code %q was dropped", code)
		}
	}
	for _, subtype := range []string{"error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries"} {
		if ResultSubtype(subtype) != subtype {
			t.Errorf("schema result subtype %q was dropped", subtype)
		}
	}
	// Anything else may be provider text, including text shaped like a code.
	for _, value := range []string{"", "success", "Rate_Limit", "rate_limit ", "your key sk-ant-123 is invalid", "error_future_subtype"} {
		if ErrorCode(value) != "" || ResultSubtype(value) != "" {
			t.Errorf("%q was retained", value)
		}
	}
}

func TestRejectionBoundsTheStatedReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for name, tc := range map[string]struct {
		info     string
		ok       bool
		rejected bool
		resets   bool
	}{
		"allowed":        {`{"status":"allowed","resetsAt":1800003600}`, true, false, false},
		"rejected":       {`{"status":"rejected","resetsAt":1800003600}`, true, true, true},
		"no reset":       {`{"status":"rejected"}`, true, true, false},
		"past reset":     {`{"status":"rejected","resetsAt":1799990000}`, true, true, false},
		"distant reset":  {`{"status":"rejected","resetsAt":1900000000}`, true, true, false},
		"unknown status": {`{"status":"paused"}`, false, false, false},
		"not an object":  {`[]`, false, false, false},
	} {
		limit, ok := Rejection(json.RawMessage(tc.info), now)
		if ok != tc.ok || limit.Rejected != tc.rejected || (limit.ResetsAt != nil) != tc.resets {
			t.Fatalf("%s: %+v %v", name, limit, ok)
		}
	}
}

func TestRefusalSaysWhyARequestFailed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var r Refusal
	if cause, _, _ := r.Cause(); cause != "" || r.Code() != "" {
		t.Fatal("nothing refused, yet a cause")
	}
	r.Assistant("rate_limit")
	if cause, resets, transient := r.Cause(); cause != harness.CauseRateLimited || resets != nil || !transient {
		t.Fatalf("an API rate limit: %v %v %v", cause, resets, transient)
	}
	// A rejection explains only a rate_limit refusal.
	if !r.RateLimit(json.RawMessage(`{"status":"rejected","resetsAt":1800003600}`), now) {
		t.Fatal("rejection unread")
	}
	if cause, resets, transient := r.Cause(); cause != harness.CauseQuotaExhausted || resets == nil || transient {
		t.Fatalf("a subscription window: %v %v %v", cause, resets, transient)
	}
	r.Assistant("overloaded")
	if cause, _, transient := r.Cause(); cause != harness.CauseOverloaded || !transient {
		t.Fatalf("overloaded under a rejection: %v", cause)
	}
	if r.RateLimit(json.RawMessage(`{"status":"surprise"}`), now) {
		t.Fatal("an unknown status was read")
	}
	r.Assistant("secret prose")
	if cause, _, _ := r.Cause(); r.Code() != "" || cause != "" {
		t.Fatal("prose was kept as a code")
	}
	r.Assistant("invalid_request")
	if cause, _, _ := r.Cause(); cause != harness.CauseUnknown || r.Code() != "invalid_request" {
		t.Fatalf("an unclassified enum: %v", cause)
	}
}

func TestThinkingIsAPartOfOutput(t *testing.T) {
	three, nine, negative := int64(3), int64(9), int64(-1)
	for name, tc := range map[string]struct {
		details *OutputDetails
		ok      bool
	}{"stated": {&OutputDetails{&three}, true}, "beyond output": {&OutputDetails{&nine}, false}, "negative": {&OutputDetails{&negative}, false}, "absent": {&OutputDetails{}, false}, "no details": {nil, false}} {
		if n, ok := tc.details.Reasoning(4); ok != tc.ok || (ok && n != 3) {
			t.Errorf("%s: %d %v", name, n, ok)
		}
	}
}
