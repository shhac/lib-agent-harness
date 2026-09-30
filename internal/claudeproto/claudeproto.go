// Package claudeproto holds what completion, sessions and native runs all read
// from Claude's stream-json: the enum vocabularies they turn into typed
// failure codes, what a refused request means, and how its usage splits. The
// lists are a redaction boundary as much as a vocabulary: a value outside them
// could be provider prose in a malformed or future frame, so it is never
// retained. One copy keeps the packages from disagreeing about what is safe to
// report or what a refusal was.
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

// causeOf classifies an assistant error. Transient marks rejections that can
// clear by waiting: only a check that has seen the whole stream, and found no
// partial output, may treat those as permission to retry.
func causeOf(code string) (cause harness.Cause, transient bool) {
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

// rateLimit is what a rate_limit_event said about the request it precedes.
// Claude sends one only for a subscription login, ahead of the error a refused
// request produces. Rejected means a subscription limit refused it; ResetsAt is
// that limit's stated reset, nil when absent or implausible.
type rateLimit struct {
	Rejected bool
	ResetsAt *time.Time
}

// rejection reads a rate_limit_event frame's rate_limit_info. False means the
// frame is not one this library understands.
func rejection(info json.RawMessage, now time.Time) (rateLimit, bool) {
	var r struct {
		Status   string `json:"status"`
		ResetsAt *int64 `json:"resetsAt"`
	}
	if json.Unmarshal(info, &r) != nil {
		return rateLimit{}, false
	}
	switch r.Status {
	case "allowed", "allowed_warning":
		return rateLimit{}, true
	case "rejected":
	default:
		return rateLimit{}, false
	}
	limit := rateLimit{Rejected: true}
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

// Refusal follows what Claude said about the latest refused request: the enum
// on the synthetic assistant message it produces, and the rate_limit_event a
// subscription login is sent before it. A later frame replaces an earlier one,
// and a response the model actually gave clears the enum, since the CLI
// recovered from the refusal.
type Refusal struct {
	code  string
	limit rateLimit
}

// Assistant records an assistant frame's error field; an ordinary response
// has none, which clears any earlier refusal.
func (r *Refusal) Assistant(errorField string) { r.code = ErrorCode(errorField) }

// RateLimit records a rate_limit_event's rate_limit_info. False means the
// frame is not one this library understands, and nothing was recorded.
func (r *Refusal) RateLimit(info json.RawMessage, now time.Time) bool {
	limit, ok := rejection(info, now)
	if ok {
		r.limit = limit
	}
	return ok
}

// Code is the refused request's error enum; empty when there was none.
func (r Refusal) Code() string { return r.code }

// Cause says why the refused request failed, and is empty when nothing was
// refused. A subscription window that refused it is an exhausted quota, with
// that window's reset, although the CLI calls it a rate limit. Transient marks
// a cause that can clear by waiting, which only a check that has seen the
// whole stream may treat as permission to retry.
func (r Refusal) Cause() (cause harness.Cause, resetsAt *time.Time, transient bool) {
	if r.code == "" {
		return "", nil, false
	}
	if r.limit.Rejected && r.code == "rate_limit" {
		return harness.CauseQuotaExhausted, r.limit.ResetsAt, false
	}
	cause, transient = causeOf(r.code)
	return cause, nil, transient
}

// OutputDetails is the output_tokens_details Claude reports beside a usage's
// output_tokens.
type OutputDetails struct {
	Thinking *int64 `json:"thinking_tokens"`
}

// Reasoning is how much of output was thinking, which Claude counts as a part
// of output_tokens. A figure absent, negative or larger than its whole is not
// one, and leaves reasoning unreported without discarding the usage.
func (d *OutputDetails) Reasoning(output int64) (int64, bool) {
	if d == nil || d.Thinking == nil || *d.Thinking < 0 || *d.Thinking > output {
		return 0, false
	}
	return *d.Thinking, true
}
