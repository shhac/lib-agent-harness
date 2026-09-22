// Package claudeproto holds the Claude stream-json enum vocabularies that
// completion and sessions both turn into typed failure codes. The lists are a
// redaction boundary as much as a vocabulary: a value outside them could be
// provider prose in a malformed or future frame, so it is never retained. One
// copy keeps the two packages from disagreeing about what is safe to report.
package claudeproto

// ErrorCode returns code when it is an assistant error the native schema
// defines, and an empty string otherwise.
func ErrorCode(code string) string {
	switch code {
	case "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "verification_required",
		"billing_error", "rate_limit", "overloaded", "invalid_request", "model_not_found",
		"server_error", "unknown", "max_output_tokens", "cloud_credential_error":
		return code
	}
	return ""
}

// ResultSubtype returns subtype when it is an error result subtype the native
// schema defines, and an empty string otherwise.
func ResultSubtype(subtype string) string {
	switch subtype {
	case "error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries":
		return subtype
	}
	return ""
}
