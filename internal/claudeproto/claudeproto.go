// Package claudeproto holds the Claude stream-json enum vocabularies that
// completion and sessions both turn into typed failure codes. The lists are a
// redaction boundary as much as a vocabulary: a value outside them could be
// provider prose in a malformed or future frame, so it is never retained. One
// copy keeps the two packages from disagreeing about what is safe to report.
package claudeproto

import (
	"encoding/json"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// resetHorizon bounds a stated reset: the longest limit Claude reports is a
// monthly overage period. A time outside it is a clock or protocol fault, and
// reporting it would send a caller to sleep on a guess.
const resetHorizon = 35 * 24 * time.Hour

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

// Cause classifies an assistant error. Transient marks rejections that can
// clear by waiting: only a check that has seen the whole stream, and found no
// partial output, may treat those as permission to retry.
func Cause(code string) (cause harness.Cause, transient bool) {
	switch code {
	case "rate_limit":
		return harness.CauseRateLimited, true
	case "overloaded":
		return harness.CauseOverloaded, true
	case "server_error":
		return harness.CauseUnavailable, true
	case "authentication_failed", "cloud_credential_error":
		return harness.CauseAuthentication, false
	case "oauth_org_not_allowed", "account_on_hold", "verification_required":
		return harness.CausePermissionDenied, false
	case "model_not_found":
		return harness.CauseModelUnavailable, false
	case "billing_error":
		return harness.CauseQuotaExhausted, false
	case "max_output_tokens":
		return harness.CauseOutputTruncated, false
	}
	return harness.CauseUnknown, false
}

// Limit is what a rate_limit_event said about the request it precedes.
// Claude sends one only for a subscription login, ahead of the error a refused
// request produces. Rejected means a subscription limit refused it; ResetsAt is
// that limit's stated reset, nil when absent or implausible.
type Limit struct {
	Rejected bool
	ResetsAt *time.Time
}

// Rejection reads a rate_limit_event frame's rate_limit_info. False means the
// frame is not one this library understands.
func Rejection(info json.RawMessage, now time.Time) (Limit, bool) {
	var r struct {
		Status   string `json:"status"`
		ResetsAt *int64 `json:"resetsAt"`
	}
	if json.Unmarshal(info, &r) != nil {
		return Limit{}, false
	}
	switch r.Status {
	case "allowed", "allowed_warning":
		return Limit{}, true
	case "rejected":
	default:
		return Limit{}, false
	}
	limit := Limit{Rejected: true}
	if r.ResetsAt != nil {
		limit.ResetsAt = Reset(*r.ResetsAt, now)
	}
	return limit, true
}

// Reset is a stated reset time in Unix seconds, or nil when it is already
// past or further away than any limit Claude reports.
func Reset(unix int64, now time.Time) *time.Time {
	when := time.Unix(unix, 0).UTC()
	if when.Before(now.Add(-time.Minute)) || when.After(now.Add(resetHorizon)) {
		return nil
	}
	return &when
}

// Explains reports whether this limit is the reason for an assistant error: a
// subscription window refused the request, so it will not clear by waiting a
// moment although the CLI calls it a rate limit.
func (l Limit) Explains(assistantError string) bool {
	return l.Rejected && assistantError == "rate_limit"
}
