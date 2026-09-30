package claudeproto

import (
	"encoding/json"
	"testing"
	"time"
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
	if !(Limit{Rejected: true}).Explains("rate_limit") || (Limit{Rejected: true}).Explains("overloaded") || (Limit{}).Explains("rate_limit") {
		t.Fatal("a rejection explains only a rate_limit refusal")
	}
}
