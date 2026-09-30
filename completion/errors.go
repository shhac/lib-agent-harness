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

// claudeTerminalFailure classifies a failed result. Only causes that cannot
// clear by waiting are set here; a transient one needs the whole stream to
// have been nothing but a refusal (see claudeFailure).
func claudeTerminalFailure(subtype, reason, stop string, refusal claudeproto.Refusal) *RequestError {
	f := &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: claudeproto.ResultSubtype(subtype)}
	if code := refusal.Code(); code != "" {
		f.Code = code
	}
	if cause, resetsAt, transient := refusal.Cause(); cause != "" && !transient {
		f.Cause, f.ResetsAt = cause, resetsAt
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

// claudeFailure reads a Claude stream that ended in an errored result: the
// failure, and whether the stream was nothing but a refusal. A failure may
// follow partial output, so it names only causes that cannot clear by
// waiting. Only a refusal-only stream (an init with no native tools, one
// synthetic assistant error, rate-limit events and one errored result) says
// no work happened, so only it may carry a transient cause and with it
// permission to retry. Anything after the result, or a line that cannot be
// read, and it is not a failure this library classifies.
func claudeFailure(data []byte) (*RequestError, bool) {
	var refusal claudeproto.Refusal
	var failure *RequestError
	var subtype, reason string
	only, refused := true, false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if failure != nil {
			return nil, false
		}
		var e struct {
			Type, Subtype, Error string
			IsError              bool            `json:"is_error"`
			Reason               string          `json:"terminal_reason"`
			Stop                 string          `json:"stop_reason"`
			Info                 json.RawMessage `json:"rate_limit_info"`
		}
		if json.Unmarshal(line, &e) != nil {
			return nil, false
		}
		switch e.Type {
		case "assistant":
			refusal.Assistant(e.Error)
			only = only && !refused && e.Error != "" && textOnly(line)
			refused = true
		case "rate_limit_event":
			only = refusal.RateLimit(e.Info, time.Now()) && only
		case "result":
			// The CLI reports a refused request as an errored result whose
			// subtype is "success"; is_error is what marks it failed.
			if !e.IsError {
				return nil, false
			}
			failure = claudeTerminalFailure(e.Subtype, e.Reason, e.Stop, refusal)
			subtype, reason = e.Subtype, e.Reason
			only = only && refusedResult(line, e.Subtype, e.Reason)
		case "system":
			only = only && e.Subtype == "init" && structuredOnlyInit(line)
		default:
			only = false
		}
	}
	if failure == nil {
		return nil, false
	}
	only = only && refused
	// Older CLIs labelled a refusal error_during_execution; current ones say
	// success with terminal_reason api_error.
	if cause, _, transient := refusal.Cause(); only && transient && failure.Cause == harness.CauseUnknown && (subtype == "error_during_execution" || reason == "api_error") {
		failure.Cause = cause
	}
	return failure, only
}

// textOnly reports an assistant frame whose content is text: a tool call or
// thinking is work, not a refusal.
func textOnly(line []byte) bool {
	var frame struct {
		Message struct {
			Content []struct{ Type string } `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return false
	}
	for _, block := range frame.Message.Content {
		if block.Type != "text" {
			return false
		}
	}
	return true
}

// refusedResult reports an errored result that carries no report and says
// the API refused the request.
func refusedResult(line []byte, subtype, reason string) bool {
	var frame struct {
		Structured json.RawMessage `json:"structured_output"`
	}
	return json.Unmarshal(line, &frame) == nil && len(frame.Structured) == 0 && (subtype != "success" || reason == "api_error")
}

// structuredOnlyInit reports an init frame offering no native tool.
func structuredOnlyInit(line []byte) bool {
	var frame struct {
		Tools []string `json:"tools"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return false
	}
	for _, tool := range frame.Tools {
		if tool != "StructuredOutput" {
			return false
		}
	}
	return true
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
