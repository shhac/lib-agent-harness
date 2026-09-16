package session

import (
	"encoding/json"
	"strings"
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
			s.invalidateContext(t, "context compacted; awaiting a fresh observation")
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
	if r.Usage != nil && !r.IsError && validClaudeUsage(m["usage"]) {
		t.result.Usage = Usage{Known: true, Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheWrite}
	}
	usage := t.result.Usage
	t.mu.Unlock()
	if r.IsError {
		if interrupted && r.Subtype == "error_during_execution" {
			status = "interrupted"
		} else {
			status = "failed"
			err = ErrTurnFailed
		}
	}
	s.claudeModelCapacity(t, m["modelUsage"])
	if usage.Known {
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
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
