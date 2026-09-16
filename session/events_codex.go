package session

import "encoding/json"

// Codex's app-server dialect. Kept apart from Claude's because the two
// protocols evolve independently; in particular the usage rule here (sum each
// model response within this turn) is the opposite of Claude's.

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
