package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/internal/claudeproto"
)

// ErrorKind identifies a failure without retaining provider text or credentials.
type ErrorKind string

const (
	ErrorOverloaded            ErrorKind = "overloaded"
	ErrorRateLimited           ErrorKind = "rate_limited"
	ErrorUnavailable           ErrorKind = "unavailable"
	ErrorAuthentication        ErrorKind = "authentication"
	ErrorContextLimit          ErrorKind = "context_limit"
	ErrorModelUnavailable      ErrorKind = "model_unavailable"
	ErrorStructuredOutputLimit ErrorKind = "structured_output_limit"
	ErrorPermissionDenied      ErrorKind = "permission_denied"
	ErrorTimeout               ErrorKind = "timeout"
	ErrorUnknown               ErrorKind = "unknown"
)

// ErrorPhase identifies where a failure was observed, not whether it spent quota.
type ErrorPhase string

const (
	PhasePreflight ErrorPhase = "preflight"
	PhaseProcess   ErrorPhase = "process"
	PhaseResponse  ErrorPhase = "response"
)

// RequestError describes a failed inference. RetryAfter is zero when the native
// protocol supplies no trustworthy delay. The library never retries requests.
type RequestError struct {
	Kind       ErrorKind
	RetryAfter time.Duration
	// Engine, Phase and Code contain only library constants or recognized native
	// enums. Unknown native values are omitted, never copied from provider text.
	Engine string
	Phase  ErrorPhase
	Code   string
	// ExitCode is present only when the process actually exited with this status.
	// A signal-killed process may report -1. It does not establish request outcome.
	ExitCode *int
}

func (e *RequestError) Error() string {
	if detail := e.diagnosticDetail(); detail != "" {
		return detail
	}
	switch e.Kind {
	case ErrorOverloaded:
		return "model provider overloaded"
	case ErrorRateLimited:
		return "model provider rate limited"
	case ErrorUnavailable:
		return "model provider unavailable"
	case ErrorAuthentication:
		return "model authentication failed"
	case ErrorContextLimit:
		return "model context limit reached"
	case ErrorModelUnavailable:
		return "selected model is unavailable; check the installed CLI model catalog and account access"
	case ErrorStructuredOutputLimit:
		return "model exhausted structured output attempts"
	case ErrorPermissionDenied:
		return "model request permission denied"
	case ErrorTimeout:
		return "model request timed out; outcome or usage may be unknown"
	default:
		return "model request failed; outcome or usage may be unknown"
	}
}

// Unwrap preserves deadline detection without retaining a raw subprocess error.
func (e *RequestError) Unwrap() error {
	if e != nil && e.Kind == ErrorTimeout {
		return context.DeadlineExceeded
	}
	return nil
}

// processRequestFailure records only safe process facts. A timeout/cancellation
// cannot borrow retry permission from output printed before the process stopped.
func processRequestFailure(engine string, data []byte, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	failure := &RequestError{Kind: ErrorUnknown, Engine: engine, Phase: PhaseProcess}
	if errors.Is(err, context.DeadlineExceeded) {
		failure.Kind, failure.Code = ErrorTimeout, "deadline_exceeded"
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
			if engine == "claude" {
				terminal = claudeRequestFailure(data)
			} else {
				terminal = codexRequestFailure(data)
			}
			if terminal == nil && engine == "claude" {
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

func claudeTerminalFailure(subtype, reason, stop, assistantError string) *RequestError {
	f := &RequestError{Kind: ErrorUnknown, Engine: "claude", Phase: PhaseResponse, Code: claudeproto.ResultSubtype(subtype)}
	if code := claudeproto.ErrorCode(assistantError); code != "" {
		f.Code = code
	}
	switch assistantError {
	case "authentication_failed", "cloud_credential_error":
		f.Kind = ErrorAuthentication
	case "oauth_org_not_allowed", "account_on_hold", "verification_required":
		f.Kind = ErrorPermissionDenied
	case "model_not_found":
		f.Kind = ErrorModelUnavailable
	}
	// These terminal facts take precedence over earlier assistant errors. None
	// permit automatic retry even if a transient rejection occurred earlier.
	if subtype == "error_max_structured_output_retries" || reason == "structured_output_retry_exhausted" {
		f.Kind, f.Code = ErrorStructuredOutputLimit, "error_max_structured_output_retries"
	}
	if reason == "prompt_too_long" || stop == "model_context_window_exceeded" {
		f.Kind, f.Code = ErrorContextLimit, "model_context_window_exceeded"
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
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if result != nil {
			return nil
		}
		var e struct {
			Type, Subtype, Error string
			IsError              bool   `json:"is_error"`
			Reason               string `json:"terminal_reason"`
			Stop                 string `json:"stop_reason"`
		}
		if json.Unmarshal(line, &e) != nil {
			return nil
		}
		if e.Type == "assistant" {
			assistantError = claudeproto.ErrorCode(e.Error)
		}
		if e.Type == "result" {
			if !e.IsError || e.Subtype == "success" {
				return nil
			}
			result = claudeTerminalFailure(e.Subtype, e.Reason, e.Stop, assistantError)
		}
	}
	return result
}

// Retryable reports only explicit provider transient rejections with no partial
// output. It is permission to apply caller retry policy, not proof of zero spend.
func (e *RequestError) Retryable() bool {
	return e != nil && (e.Kind == ErrorOverloaded || e.Kind == ErrorRateLimited || e.Kind == ErrorUnavailable)
}

// Claude emits synthetic assistant error messages followed by an error result.
// Require both, scan the ENTIRE output, and reject any successful/partial output.
// Unknown result text, transport failures and malformed frames are not evidence
// of an overload.
func claudeRequestFailure(data []byte) *RequestError {
	kind := ErrorUnknown
	var assistantError, subtype, reason, stop string
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
			switch e.Error {
			case "rate_limit":
				kind = ErrorRateLimited
			case "overloaded":
				kind = ErrorOverloaded
			case "server_error":
				kind = ErrorUnavailable
			case "authentication_failed":
				kind = ErrorAuthentication
			}
		case "result":
			if !e.IsError || e.Subtype == "success" || len(e.Structured) > 0 {
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
	if failed && marked {
		failure := claudeTerminalFailure(subtype, reason, stop, assistantError)
		if failure.Kind == ErrorUnknown && subtype == "error_during_execution" {
			failure.Kind = kind
		}
		return failure
	}
	return nil
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
	kind := ErrorUnknown
	code := "turn.failed"
	switch terminal {
	case "Selected model is at capacity. Please try a different model.":
		kind, code = ErrorOverloaded, "model_capacity"
	case "Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.":
		kind, code = ErrorContextLimit, "context_window_exceeded"
	default:
		for _, status := range []struct {
			code string
			kind ErrorKind
		}{
			{"429 Too Many Requests", ErrorRateLimited}, {"503 Service Unavailable", ErrorUnavailable}, {"529 <unknown status code>", ErrorOverloaded},
			{"401 Unauthorized", ErrorAuthentication}, {"403 Forbidden", ErrorPermissionDenied},
		} {
			if strings.HasPrefix(terminal, "unexpected status "+status.code+": ") || terminal == "exceeded retry limit, last status: "+status.code || strings.HasPrefix(terminal, "exceeded retry limit, last status: "+status.code+", request id: ") {
				kind, code = status.kind, "http_"+strings.Fields(status.code)[0]
				break
			}
		}
	}
	return &RequestError{Kind: kind, Engine: "codex", Phase: PhaseResponse, Code: code}
}
