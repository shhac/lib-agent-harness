package completion

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// ErrorKind identifies a failure without retaining provider text or credentials.
type ErrorKind string

const (
	ErrorOverloaded     ErrorKind = "overloaded"
	ErrorRateLimited    ErrorKind = "rate_limited"
	ErrorUnavailable    ErrorKind = "unavailable"
	ErrorAuthentication ErrorKind = "authentication"
	ErrorContextLimit   ErrorKind = "context_limit"
	ErrorUnknown        ErrorKind = "unknown"
)

// RequestError describes a failed inference. RetryAfter is zero when the native
// protocol supplies no trustworthy delay. The library never retries requests.
type RequestError struct {
	Kind       ErrorKind
	RetryAfter time.Duration
}

func (e *RequestError) Error() string {
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
	default:
		return "model request failed; outcome or usage may be unknown"
	}
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
		return &RequestError{Kind: kind}
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
	switch terminal {
	case "Selected model is at capacity. Please try a different model.":
		kind = ErrorOverloaded
	case "Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.":
		kind = ErrorContextLimit
	default:
		for _, status := range []struct {
			code string
			kind ErrorKind
		}{
			{"429 Too Many Requests", ErrorRateLimited}, {"503 Service Unavailable", ErrorUnavailable}, {"529 <unknown status code>", ErrorOverloaded},
			{"401 Unauthorized", ErrorAuthentication},
		} {
			if strings.HasPrefix(terminal, "unexpected status "+status.code+": ") || terminal == "exceeded retry limit, last status: "+status.code || strings.HasPrefix(terminal, "exceeded retry limit, last status: "+status.code+", request id: ") {
				kind = status.kind
				break
			}
		}
	}
	return &RequestError{Kind: kind}
}
