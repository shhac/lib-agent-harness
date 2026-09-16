package session

import (
	"encoding/json"
	"strings"
)

func (s *Session) notification(m map[string]json.RawMessage) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.notificationLocked(m)
}
func (s *Session) notificationLocked(m map[string]json.RawMessage) {
	s.mu.Lock()
	t := s.active
	ref := s.ref
	s.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	if t.starting {
		size := len(mustMarshal(m))
		if len(t.pending) >= s.options.EventBuffer {
			t.mu.Unlock()
			s.fail(ErrBackpressure)
			return
		}
		if t.pendingBytes+size > MaxFrameBytes {
			t.mu.Unlock()
			s.fail(ErrOutputLimit)
			return
		}
		t.pending = append(t.pending, m)
		t.pendingBytes += size
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	if s.options.Engine == Codex {
		s.codexEvent(t, ref, m)
	} else {
		s.claudeEvent(t, ref, m)
	}
}
func (s *Session) emit(t *Turn, e Event) {
	if err := t.emit(e); err != nil {
		s.fail(err)
	}
}
func (s *Session) text(t *Turn, item, text string, replace bool) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	if replace || t.textItem != item {
		t.result.Text = ""
		t.textItem = item
	}
	if len(t.result.Text)+len(text) > s.options.MaxTextBytes {
		t.mu.Unlock()
		s.fail(ErrOutputLimit)
		return
	}
	t.result.Text += text
	t.mu.Unlock()
	kind := "text_delta"
	if replace {
		kind = "text"
	}
	s.emit(t, Event{Kind: kind, ItemID: item, Text: text})
}
func (s *Session) codexEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	var p map[string]json.RawMessage
	if json.Unmarshal(m["params"], &p) != nil {
		return
	}
	if thread := str(p, "threadId"); thread != "" && ref.ID != "" && thread != ref.ID {
		return
	}
	method := str(m, "method")
	if method == "turn/started" {
		return
	}
	if id := str(p, "turnId"); id != "" && id != t.ID() {
		return
	}
	switch method {
	case "item/agentMessage/delta":
		s.text(t, str(p, "itemId"), str(p, "delta"), false)
	case "item/started", "item/completed":
		var item map[string]json.RawMessage
		if json.Unmarshal(p["item"], &item) != nil {
			return
		}
		typ := str(item, "type")
		id := str(item, "id")
		if typ == "agentMessage" {
			if method == "item/completed" {
				s.text(t, id, str(item, "text"), true)
			}
			return
		}
		switch typ {
		case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "subAgentActivity", "sleep", "imageGeneration", "webSearch", "imageView":
			tool := typ
			if name := str(item, "tool"); name != "" {
				tool = name
			}
			kind := "tool_started"
			if method == "item/completed" {
				kind = "tool_completed"
			}
			s.emit(t, Event{Kind: kind, ItemID: id, Tool: tool, Status: str(item, "status")})
		}
	case "thread/tokenUsage/updated":
		var envelope struct {
			Last  json.RawMessage `json:"last"`
			Total json.RawMessage `json:"total"`
		}
		if json.Unmarshal(p["tokenUsage"], &envelope) != nil || str(p, "turnId") == "" {
			return
		}
		last, lastOK := parseCodexUsage(envelope.Last)
		total, totalOK := parseCodexUsage(envelope.Total)
		if !lastOK || !totalOK {
			return
		}
		u := struct{ Last, Total codexUsage }{last, total}
		// last is one model response; total identifies repeated notifications. Sum
		// response usage within this turn, never the resumed session's old total.
		t.mu.Lock()
		if t.lastCodexTotal != u.Total || !t.result.Usage.Known {
			last := u.Last.normalized()
			t.result.Usage.Known = true
			t.result.Usage.Input += last.Input
			t.result.Usage.Output += last.Output
			t.result.Usage.CacheRead += last.CacheRead
			t.result.Usage.CacheWrite += last.CacheWrite
			t.result.Usage.Reasoning += last.Reasoning
			t.lastCodexTotal = u.Total
		}
		usage := t.result.Usage
		t.mu.Unlock()
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	case "turn/completed":
		var turn struct{ ID, Status string }
		if json.Unmarshal(p["turn"], &turn) != nil {
			return
		}
		if turn.ID != t.ID() {
			return
		}
		var err error
		if turn.Status == "failed" {
			t.mu.Lock()
			t.result.NativeError = true
			t.mu.Unlock()
			err = ErrTurnFailed
		}
		switch turn.Status {
		case "completed", "interrupted", "failed":
		default:
			s.fail(ErrProtocol)
			return
		}
		s.emit(t, Event{Kind: "status", Status: turn.Status})
		t.finish(turn.Status, err)
	}
}

type codexUsage struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cachedInputTokens"`
	CacheWrite int64 `json:"cacheWriteInputTokens"`
	Reasoning  int64 `json:"reasoningOutputTokens"`
}

func parseCodexUsage(raw json.RawMessage) (codexUsage, bool) {
	var fields map[string]json.RawMessage
	var u codexUsage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &u) != nil {
		return u, false
	}
	for _, key := range []string{"inputTokens", "outputTokens", "cachedInputTokens", "reasoningOutputTokens"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return u, false
		}
	}
	return u, u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheRead <= u.Input && u.CacheWrite >= 0 && u.Reasoning >= 0 && u.Reasoning <= u.Output
}
func (u codexUsage) normalized() Usage {
	return Usage{Known: true, Input: max(0, u.Input-u.CacheRead), Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning}
}
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
	case "stream_event":
		var event map[string]json.RawMessage
		if json.Unmarshal(m["event"], &event) != nil {
			return
		}
		switch str(event, "type") {
		case "message_start":
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
	case "assistant":
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
	case "user":
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
	case "result":
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
		if usage.Known {
			s.emit(t, Event{Kind: "usage", Usage: &usage})
		}
		s.emit(t, Event{Kind: "status", Status: status})
		t.finish(status, err)
	}
}
func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }

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
