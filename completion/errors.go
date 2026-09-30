package completion

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// ErrorPhase identifies where a failure was observed, not whether it spent quota.
type ErrorPhase string

const (
	PhasePreflight ErrorPhase = "preflight"
	PhaseProcess   ErrorPhase = "process"
	// PhaseTransport is an API request that failed before any response status.
	PhaseTransport ErrorPhase = "transport"
	PhaseResponse  ErrorPhase = "response"
)

// RequestError describes a failed inference. RetryAfter is zero when the native
// protocol supplies no trustworthy delay. The library never retries requests.
type RequestError struct {
	Cause      harness.Cause
	RetryAfter time.Duration
	// Engine, Phase and Code contain only library constants or recognized native
	// enums. Unknown native values are omitted, never copied from provider text.
	Engine harness.Engine
	Phase  ErrorPhase
	Code   string
	// ExitCode is present only when the process actually exited with this status.
	// A signal-killed process may report -1. It does not establish request outcome.
	ExitCode *int
	// ResetsAt is when an exhausted quota resets, where the provider stated it.
	ResetsAt *time.Time
}

func (e *RequestError) Error() string {
	if detail := e.diagnosticDetail(); detail != "" {
		return detail
	}
	switch e.Cause {
	case harness.CauseOverloaded:
		return "model provider overloaded"
	case harness.CauseRateLimited:
		return "model provider rate limited"
	case harness.CauseUnavailable:
		return "model provider unavailable"
	case harness.CauseAuthentication:
		return "model authentication failed"
	case harness.CauseContextLimit:
		return "model context limit reached"
	case harness.CauseModelUnavailable:
		if e.Engine == harness.OpenAICompatible {
			return "selected model is unavailable at the configured endpoint; check the model name and account access"
		}
		return "selected model is unavailable; check the installed CLI model catalog and account access"
	case harness.CauseStructuredOutputLimit:
		return "model exhausted structured output attempts"
	case harness.CausePermissionDenied:
		return "model request permission denied"
	case harness.CauseTimeout:
		return "model request timed out; outcome or usage may be unknown"
	case harness.CauseQuotaExhausted:
		return "model provider quota exhausted"
	case harness.CauseOutputTruncated:
		return "model output reached its token limit before the response finished"
	case harness.CauseContentFiltered:
		return "model provider withheld the response under its content policy"
	default:
		return "model request failed; outcome or usage may be unknown"
	}
}

// Unwrap preserves deadline detection without retaining a raw subprocess error.
func (e *RequestError) Unwrap() error {
	if e != nil && e.Cause == harness.CauseTimeout {
		return context.DeadlineExceeded
	}
	return nil
}

// processRequestFailure records only safe process facts. A timeout/cancellation
// cannot borrow retry permission from output printed before the process stopped.
func processRequestFailure(engine harness.Engine, data []byte, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	failure := &RequestError{Cause: harness.CauseUnknown, Engine: engine, Phase: PhaseProcess}
	if errors.Is(err, context.DeadlineExceeded) {
		failure.Cause, failure.Code = harness.CauseTimeout, "deadline_exceeded"
		return failure
	}
	if errors.Is(err, errOutputLimit) {
		failure.Code = "output_limit"
		return failure
	}
	if start := startFailure(engine, PhaseProcess, err); start != nil {
		return start
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if code > 0 {
			terminal := codexRequestFailure(data)
			if engine == harness.Claude {
				terminal, _ = claudeFailure(data)
			}
			if terminal != nil {
				failure = terminal
			}
		}
		failure.ExitCode = &code
	}
	return failure
}

// Retryable reports only explicit provider transient rejections with no partial
// output. It is permission to apply caller retry policy, not proof of zero spend.
func (e *RequestError) Retryable() bool {
	return e != nil && (e.Cause == harness.CauseOverloaded || e.Cause == harness.CauseRateLimited || e.Cause == harness.CauseUnavailable)
}

// HarnessFacts reports the failure in the vocabulary every mode shares.
func (e *RequestError) HarnessFacts() harness.Facts {
	return harness.Facts{
		Engine:     e.Engine,
		Operation:  harness.Complete,
		Family:     e.family(),
		Cause:      e.Cause,
		Phase:      string(e.Phase),
		Code:       e.Code,
		ExitCode:   e.ExitCode,
		RetryAfter: e.RetryAfter,
		ResetsAt:   e.ResetsAt,
		Retryable:  e.Retryable(),
	}
}

// family keeps capability for refusals made before any request: a native
// tool seen in a response is the same fault, but that request may have been
// billed, which a capability failure promises it was not.
func (e *RequestError) family() harness.Family {
	if capabilityCode(e.Code) && e.Phase == PhasePreflight {
		return harness.FailureCapability
	}
	switch e.Phase {
	case PhasePreflight:
		return harness.FailurePreflight
	case PhaseProcess:
		return harness.FailureProcess
	}
	return harness.FailureRequest
}

// capabilityCode names refusals the same configuration will always repeat:
// the engine cannot complete here, the installed CLI's tool surface could not
// be proven closed, or the endpoint or model does not offer what was asked.
// Timeouts, missing files and credential failures can clear, so they are not.
func capabilityCode(code string) bool {
	switch code {
	case "unsupported_engine",
		"unexpected_native_tool", "unexpected_native_tool_catalog", "unexpected_native_tool_call",
		"unexpected_native_instructions", "missing_native_tool_catalog", "missing_native_transcript",
		"grok_platform_unsupported", "probe_missing_tool_catalog", "probe_transcript_unverified",
		"probe_mismatch", "probe_invalid_request", "probe_unexpected_tools", "probe_invalid_schema",
		"probe_changed_schema", "probe_changed_effort", "probe_instruction_type",
		"probe_unexpected_instructions", "probe_missing_instructions", "probe_changed_model",
		"missing_effort_catalog", "unsupported_effort", "default_effort_unlisted",
		"api_dialect_unsupported", "api_effort_parameter_unsupported",
		"global_skills_unsupported", "skills_unsupported",
		"max_output_tokens_unsupported", "probe_changed_max_output_tokens":
		return true
	}
	return false
}
