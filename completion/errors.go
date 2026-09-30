package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/claudeproto"
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
			var terminal *RequestError
			if engine == harness.Claude {
				terminal = claudeRequestFailure(data)
			} else {
				terminal = codexRequestFailure(data)
			}
			if terminal == nil && engine == harness.Claude {
				terminal = claudeTerminalDiagnostic(data)
			}
			if terminal != nil {
				failure = terminal
			}
		}
		failure.ExitCode = &code
	}
	return failure
}

// observeLimit reads a rate_limit_event frame into limit; false means it was
// not one this library understands. A later frame replaces an earlier one.
func observeLimit(limit *claudeproto.Limit, info json.RawMessage) bool {
	observed, ok := claudeproto.Rejection(info, time.Now())
	if ok {
		*limit = observed
	}
	return ok
}

// claudeTerminalFailure classifies a failed result. Only causes that cannot
// clear by waiting are set here; transient ones need the whole-stream check.
func claudeTerminalFailure(subtype, reason, stop, assistantError string, limit claudeproto.Limit) *RequestError {
	f := &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: claudeproto.ResultSubtype(subtype)}
	if code := claudeproto.ErrorCode(assistantError); code != "" {
		f.Code = code
	}
	if cause, transient := claudeproto.Cause(assistantError); !transient {
		f.Cause = cause
	}
	if limit.Explains(assistantError) {
		f.Cause, f.ResetsAt = harness.CauseQuotaExhausted, limit.ResetsAt
	}
	// These terminal facts take precedence over earlier assistant errors. None
	// permit automatic retry even if a transient rejection occurred earlier.
	if subtype == "error_max_structured_output_retries" || reason == "structured_output_retry_exhausted" {
		f.Cause, f.Code, f.ResetsAt = harness.CauseStructuredOutputLimit, "error_max_structured_output_retries", nil
	}
	if reason == "prompt_too_long" || stop == "model_context_window_exceeded" {
		f.Cause, f.Code, f.ResetsAt = harness.CauseContextLimit, "model_context_window_exceeded", nil
		if reason == "prompt_too_long" {
			f.Code = reason
		}
	}
	return f
}

// A terminal diagnostic may follow partial output, so it deliberately cannot
// classify transient rejections. Whole-stream checks for retry live separately.
func claudeTerminalDiagnostic(data []byte) *RequestError {
	var result *RequestError
	var assistantError string
	var limit claudeproto.Limit
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if result != nil {
			return nil
		}
		var e struct {
			Type, Subtype, Error string
			IsError              bool            `json:"is_error"`
			Reason               string          `json:"terminal_reason"`
			Stop                 string          `json:"stop_reason"`
			Info                 json.RawMessage `json:"rate_limit_info"`
		}
		if json.Unmarshal(line, &e) != nil {
			return nil
		}
		switch e.Type {
		case "assistant":
			assistantError = claudeproto.ErrorCode(e.Error)
		case "rate_limit_event":
			observeLimit(&limit, e.Info)
		case "result":
			// The CLI reports a refused request as an errored result whose
			// subtype is "success"; is_error is what marks it failed.
			if !e.IsError {
				return nil
			}
			result = claudeTerminalFailure(e.Subtype, e.Reason, e.Stop, assistantError, limit)
		}
	}
	return result
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
		"missing_effort_catalog", "unsupported_effort",
		"api_dialect_unsupported", "api_effort_parameter_unsupported",
		"global_skills_unsupported", "skills_unsupported",
		"max_output_tokens_unsupported", "probe_changed_max_output_tokens":
		return true
	}
	return false
}

// Claude emits synthetic assistant error messages followed by an error result.
// Require both, scan the ENTIRE output, and reject any successful/partial output.
// Unknown result text, transport failures and malformed frames are not evidence
// of an overload.
func claudeRequestFailure(data []byte) *RequestError {
	var assistantError, subtype, reason, stop string
	var limit claudeproto.Limit
	failed, marked := false, false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if failed {
			return nil
		}
		var e struct {
			Type, Subtype, Error string
			Tools                []string `json:"tools"`
			Message              struct {
				Content []struct{ Type string } `json:"content"`
			} `json:"message"`
			Reason     string          `json:"terminal_reason"`
			Stop       string          `json:"stop_reason"`
			IsError    bool            `json:"is_error"`
			Structured json.RawMessage `json:"structured_output"`
			Info       json.RawMessage `json:"rate_limit_info"`
		}
		if json.Unmarshal(line, &e) != nil {
			return nil
		}
		switch e.Type {
		case "assistant":
			for _, block := range e.Message.Content {
				if block.Type != "text" {
					return nil
				}
			}
			if e.Error == "" {
				return nil
			}
			if marked {
				return nil
			}
			marked = true
			assistantError = e.Error
		case "rate_limit_event":
			// Sent for a subscription login ahead of the refusal it explains.
			// It is not output, so it neither grants nor removes retry permission.
			if !observeLimit(&limit, e.Info) {
				return nil
			}
		case "result":
			if !e.IsError || len(e.Structured) > 0 {
				return nil
			}
			// A refused API request ends in an errored result that the CLI
			// labels "success" with terminal_reason api_error; older CLIs
			// labelled it error_during_execution.
			if e.Subtype == "success" && e.Reason != "api_error" {
				return nil
			}
			if failed {
				return nil
			}
			failed = true
			subtype, reason, stop = e.Subtype, e.Reason, e.Stop
		case "system":
			for _, tool := range e.Tools {
				if tool != "StructuredOutput" {
					return nil
				}
			}
			if e.Subtype != "init" {
				return nil
			}
		default:
			return nil
		}
	}
	if !failed || !marked {
		return nil
	}
	failure := claudeTerminalFailure(subtype, reason, stop, assistantError, limit)
	if cause, transient := claudeproto.Cause(assistantError); transient && failure.Cause == harness.CauseUnknown && (subtype == "error_during_execution" || reason == "api_error") {
		failure.Cause = cause
	}
	return failure
}

// Canonical Codex error formatting is defined by codex-rs/protocol/src/error.rs.
// The exec protocol discards typed error metadata. Match only its terminal
// envelope and exact status prefix, never arbitrary provider prose/substrings.
func codexRequestFailure(data []byte) *RequestError {
	var terminal string
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if terminal != "" {
			return nil
		}
		var e struct {
			Type  string
			Error struct{ Message string }
		}
		if json.Unmarshal(line, &e) != nil {
			return nil
		}
		switch e.Type {
		case "thread.started", "turn.started", "error":
			// error events can include CLI reconnect progress. Only turn.failed decides.
		case "turn.failed":
			if terminal != "" {
				return nil
			}
			terminal = e.Error.Message
		default:
			return nil // any item, partial response or completion prevents retry
		}
	}
	if terminal == "" {
		return nil
	}
	cause := harness.CauseUnknown
	code := "turn.failed"
	switch terminal {
	case "Selected model is at capacity. Please try a different model.":
		cause, code = harness.CauseOverloaded, "model_capacity"
	case "Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.":
		cause, code = harness.CauseContextLimit, "context_window_exceeded"
	default:
		for _, status := range []struct {
			code  string
			cause harness.Cause
		}{
			{"429 Too Many Requests", harness.CauseRateLimited}, {"503 Service Unavailable", harness.CauseUnavailable}, {"529 <unknown status code>", harness.CauseOverloaded},
			{"401 Unauthorized", harness.CauseAuthentication}, {"403 Forbidden", harness.CausePermissionDenied},
		} {
			if strings.HasPrefix(terminal, "unexpected status "+status.code+": ") || terminal == "exceeded retry limit, last status: "+status.code || strings.HasPrefix(terminal, "exceeded retry limit, last status: "+status.code+", request id: ") {
				cause, code = status.cause, "http_"+strings.Fields(status.code)[0]
				break
			}
		}
	}
	return &RequestError{Cause: cause, Engine: harness.Codex, Phase: PhaseResponse, Code: code}
}
