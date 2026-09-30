package completion

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/claudeproto"
)

// Claude failures: what a stream that ended in an errored result says, and
// whether it said nothing but that, which is what retry permission rests on.

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
