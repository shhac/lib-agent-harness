package harness

import (
	"errors"
	"time"
)

// Structured facts about a failure, for callers that record, classify or
// display their own diagnostics, whichever mode produced the error.
//
// A caller with its own error vocabulary needs to know what went wrong without
// reading a message. Parsing Error() would make this library's prose part of
// its contract, so the facts are published directly, and diagnostic fields are drawn
// from a fixed vocabulary: no harness output, provider prose, path or
// credential ever appears in one. Network-control metadata may include a validated
// IP literal and its fixed source label.

// Family says what kind of thing failed.
type Family string

const (
	// FailureCapability: the engine or installed harness cannot do what was
	// configured. Nothing was billed and no work was done; the same
	// configuration will refuse again.
	FailureCapability Family = "capability"
	// FailurePreflight: local setup failed before any request, such as a
	// missing executable, an invalid limit or an unusable credential source.
	FailurePreflight Family = "preflight"
	// FailureProcess: the harness process itself started, ended or died. An
	// exit status does not establish whether a request was billed.
	FailureProcess Family = "process"
	// FailureRequest: a provider request was refused or failed.
	FailureRequest Family = "request"
	// FailureTurn: the provider ended an agent turn in failure. Tools may
	// already have run, so this is not safe to retry silently.
	FailureTurn Family = "turn"
)

// Cause classifies why a request failed, where the provider said.
type Cause string

const (
	CauseOverloaded            Cause = "overloaded"
	CauseRateLimited           Cause = "rate_limited"
	CauseUnavailable           Cause = "unavailable"
	CauseAuthentication        Cause = "authentication"
	CauseContextLimit          Cause = "context_limit"
	CauseModelUnavailable      Cause = "model_unavailable"
	CauseStructuredOutputLimit Cause = "structured_output_limit"
	CausePermissionDenied      Cause = "permission_denied"
	CauseTimeout               Cause = "timeout"
	// CauseQuotaExhausted: a plan's usage limit, a subscription window or a
	// prepaid balance is used up. It shares 429 with rate limiting but does not
	// clear by waiting a moment; Facts.ResetsAt says when it does, where the
	// provider stated it.
	CauseQuotaExhausted Cause = "quota_exhausted"
	// CauseOutputTruncated: the response hit an output token limit and ended
	// before the model finished.
	CauseOutputTruncated Cause = "output_truncated"
	// CauseContentFiltered: the provider withheld or refused the response under
	// its content or usage policy.
	CauseContentFiltered Cause = "content_filtered"
	CauseUnknown         Cause = "unknown"
)

// Facts is what a library error knows about itself.
type Facts struct {
	Engine    Engine    `json:"engine,omitempty"`
	Operation Operation `json:"operation,omitempty"`
	Family    Family    `json:"family,omitempty"`
	Cause     Cause     `json:"cause,omitempty"`
	// Phase says how far things had got, where that is the operative
	// distinction.
	Phase string `json:"phase,omitempty"`
	// NetworkControlAddr and NetworkControlSource are sanitized DNS control
	// destination metadata, never DNS query or reply contents.
	NetworkControlAddr   string `json:"network_control_addr,omitempty"`
	NetworkControlSource string `json:"network_control_source,omitempty"`
	// ProofStep names a fixed pre-launch sandbox verification step, when known.
	ProofStep string `json:"proof_step,omitempty"`
	// Code is the specific fixed code: a capability or preflight code, a
	// process code, an HTTP status code, or a provider's enumerated result.
	Code     string `json:"code,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// RetryAfter is a provider-stated delay, bounded; zero when none was given.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// ResetsAt is when the limit that refused the request resets, as the
	// provider stated it; nil when it did not. It accompanies an exhausted
	// quota, which may be hours away, where RetryAfter is a short delay.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Retryable reports whether repeating the operation is safe on its own
	// terms. It is false for a failed turn even when the provider might
	// succeed next time, because a native turn may already have run tools.
	Retryable bool `json:"retryable"`
}

// Factual is implemented by every typed error this library returns.
type Factual interface{ HarnessFacts() Facts }

// ErrorFacts reports the structured facts an error carries, unwrapping to find
// them. False means the library did not classify it; a caller should say so
// rather than inventing a code.
func ErrorFacts(err error) (Facts, bool) {
	var carrier Factual
	if !errors.As(err, &carrier) {
		return Facts{}, false
	}
	return carrier.HarnessFacts(), true
}
