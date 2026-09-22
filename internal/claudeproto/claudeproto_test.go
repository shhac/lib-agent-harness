package claudeproto

import "testing"

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
