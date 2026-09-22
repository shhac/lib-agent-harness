package session

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/internal/claudeproto"
)

// Claude's stream-json dialect. Kept apart from Codex's because the two
// protocols evolve independently; in particular the usage rule here (take the
// result event's figures, and only for a non-errored turn) is the opposite of
// Codex's per-response summing.

// claudeEvent routes one frame. Everything it accepts belongs to this session
// and this turn; the per-type work is in the handlers below.
func (s *Session) claudeEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if id := str(m, "session_id"); id != "" && id != ref.ID {
		s.fail(ErrProtocol)
		return
	}
	// Subagent events belong to a different native turn and must not overwrite
	// this turn's answer or complete its tool cards.
	if parent := str(m, "parent_tool_use_id"); parent != "" {
		return
	}
	switch str(m, "type") {
	case "system":
		if str(m, "subtype") == "compact_boundary" {
			// Both a boundary observation and an invalidation. A caller counting
			// compactions needs to be told one happened; invalidating the occupancy
			// alone leaves it looking like the number simply moved.
			s.invalidateContext(t, "context compacted; awaiting a fresh observation")
			s.emit(t, Event{Kind: "compaction_completed", ItemID: str(m, "uuid"), Status: claudeCompactionTrigger(m)})
		}
	case "stream_event":
		s.claudeStreamEvent(t, m)
	case "assistant":
		s.claudeAssistant(t, m)
	case "user":
		s.claudeUser(t, m)
	case "result":
		s.claudeResult(t, m)
	}
}

func (s *Session) claudeStreamEvent(t *Turn, m map[string]json.RawMessage) {
	var event map[string]json.RawMessage
	if json.Unmarshal(m["event"], &event) != nil {
		return
	}
	switch str(event, "type") {
	case "message_start":
		s.claudeMessageContext(t, event["message"])
		var message struct{ ID string }
		if json.Unmarshal(event["message"], &message) == nil {
			t.mu.Lock()
			t.textItem = message.ID
			t.result.Text = ""
			t.mu.Unlock()
		}
	case "content_block_delta":
		var delta struct{ Type, Text string }
		if json.Unmarshal(event["delta"], &delta) == nil && delta.Type == "text_delta" {
			t.mu.Lock()
			id := t.textItem
			t.mu.Unlock()
			s.text(t, id, delta.Text, false)
		}
	}
}

func (s *Session) claudeAssistant(t *Turn, m map[string]json.RawMessage) {
	s.claudeMessageContext(t, m["message"])
	// One completed model response. A turn contains many, and its terminal
	// accounting arrives far too late for a caller holding a budget to act on,
	// so publish each response's figures as they are reported.
	s.observeRequestUsage(t, claudeResponseUsage(m["message"]))
	var message struct {
		ID      string
		Content []struct{ Type, ID, Name, Text string }
	}
	if json.Unmarshal(m["message"], &message) != nil {
		return
	}
	var text []string
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text = append(text, block.Text)
		case "tool_use":
			s.emit(t, Event{Kind: "tool_started", ItemID: block.ID, Tool: block.Name, Status: "running"})
		}
	}
	if len(text) > 0 {
		s.text(t, message.ID, strings.Join(text, "\n"), true)
	}
}

func (s *Session) claudeUser(t *Turn, m map[string]json.RawMessage) {
	var message struct {
		Content []struct {
			Type      string
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
		}
	}
	if json.Unmarshal(m["message"], &message) != nil {
		return
	}
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			status := "completed"
			if block.IsError {
				status = "failed"
			}
			s.emit(t, Event{Kind: "tool_completed", ItemID: block.ToolUseID, Status: status})
		}
	}
}

func (s *Session) claudeResult(t *Turn, m map[string]json.RawMessage) {
	var r struct {
		Result  string
		Subtype string
		IsError bool `json:"is_error"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		}
	}
	if json.Unmarshal(mustMarshal(m), &r) != nil || r.Subtype == "" || (!r.IsError && r.Subtype != "success") {
		s.fail(ErrProtocol)
		return
	}
	// Error result strings can include provider diagnostics/credentials; only
	// successful assistant content is included in the public result.
	if !r.IsError && r.Result != "" {
		s.text(t, "result", r.Result, true)
	}
	status := "completed"
	var err error
	t.mu.Lock()
	interrupted := t.interruptRequested
	t.result.NativeError = r.IsError
	// An errored or interrupted turn's terminal counters are not this turn's
	// accounting: the CLI has been observed carrying the preceding completed
	// turn's figures into them. They stay unknown, and what this turn actually
	// consumed survives as the per-response observations in Result.Observed —
	// evidence a caller can show, rather than a measurement it can trust.
	if r.Usage != nil && !r.IsError && validClaudeUsage(m["usage"]) {
		t.result.Usage = Usage{Known: true, Final: true, Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheWrite}
	}
	usage := t.result.Usage
	t.mu.Unlock()
	if r.IsError {
		if interrupted && r.Subtype == "error_during_execution" {
			status = "interrupted"
		} else {
			status = "failed"
			// Keep what the provider actually said about it, as fixed codes rather
			// than its text. A failure that arrives only as "the turn failed" is the
			// unexplained outcome this whole contract exists to stop producing, and
			// a protocol-level result error typically has no process output to fall
			// back on.
			err = s.claudeTerminalFailure(m, r.Subtype)
		}
	}
	s.claudeModelCapacity(t, m["modelUsage"])
	if usage.Known {
		usage.Final = true
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
}

// claudeResponseUsage reads one assistant message's reported consumption. An
// absent or unusable report is unknown, never zero.
func claudeResponseUsage(raw json.RawMessage) Usage {
	var message struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &message) != nil || len(message.Usage) == 0 || !validClaudeUsage(message.Usage) {
		return Usage{}
	}
	var counts struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	}
	if json.Unmarshal(message.Usage, &counts) != nil {
		return Usage{}
	}
	return Usage{Known: true, Input: counts.Input, Output: counts.Output, CacheRead: counts.CacheRead, CacheWrite: counts.CacheWrite}
}

func validClaudeUsage(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return false
		}
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if len(fields[key]) == 0 {
			continue
		}
		var n int64
		if json.Unmarshal(fields[key], &n) != nil || n < 0 {
			return false
		}
	}
	return true
}

// claudeCompactionTrigger reports why a compaction happened, from the frame's
// own enumerated value. An unrecognized trigger is reported as unknown rather
// than passed through: this is a status, not a place for provider text.
func claudeCompactionTrigger(m map[string]json.RawMessage) string {
	switch trigger := str(m, "compact_metadata_trigger"); trigger {
	case "auto", "manual":
		return trigger
	}
	switch trigger := str(m, "trigger"); trigger {
	case "auto", "manual":
		return trigger
	}
	return "unknown"
}

// claudeTerminalFailure builds a typed failure from a result frame, and hands
// the caller a bounded, sanitized diagnostic through the hook that exists for
// it. Provider text never enters the error value.
func (s *Session) claudeTerminalFailure(m map[string]json.RawMessage, subtype string) error {
	failure := &TurnError{Engine: string(Claude), Code: claudeproto.ResultSubtype(subtype)}
	if failure.Code == "" {
		failure.Code = "turn_failed"
	}
	var frame struct {
		Reason string `json:"terminal_reason"`
		Stop   string `json:"stop_reason"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(mustMarshal(m), &frame) == nil {
		if code := claudeproto.ErrorCode(frame.Error); code != "" {
			failure.Code = code
		}
		if frame.Reason == "prompt_too_long" || frame.Stop == "model_context_window_exceeded" {
			failure.Code = "model_context_window_exceeded"
		}
	}
	if report := s.options.OnDiagnostic; report != nil {
		report(Diagnostic{Engine: string(Claude), Stage: "turn_result", Code: failure.Code, Detail: sanitize(mustMarshal(m["errors"]), 1024), At: time.Now().UTC()})
	}
	return failure
}
