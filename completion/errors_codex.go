package completion

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/shhac/lib-agent-harness"
)

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
