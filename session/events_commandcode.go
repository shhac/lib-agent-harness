package session

import (
	"encoding/json"
	"strconv"

	harness "github.com/shhac/lib-agent-harness"
)

// Command Code's session/update dialect, and the prompt response that ends a
// turn. Every shape here was observed from Command Code 1.74.1. Its prompt
// response carries two usage scopes: usage is the session's running total,
// and _meta.usage is this turn's own, which is the one reported.

// readCommandCodePromptEnd reads Command Code's prompt response, whose
// _meta.usage is the turn's own accounting.
func readCommandCodePromptEnd(body json.RawMessage, interruptRequested bool) (acpTurnEnd, bool) {
	var r struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"_meta"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		return acpTurnEnd{}, false
	}
	status, code := acpOutcome(r.StopReason, interruptRequested)
	return acpTurnEnd{status: status, code: code, usage: parseCommandCodeTurnUsage(r.Meta.Usage)}, true
}

// commandCodeEvent routes one notification for the active turn.
func (s *Session) commandCodeEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if str(m, "method") != "session/update" {
		return
	}
	var p struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(m["params"], &p) != nil || p.Update == nil || ref.ID == "" || p.SessionID != ref.ID {
		return
	}
	switch str(p.Update, "sessionUpdate") {
	case "agent_message_chunk":
		var content struct{ Type, Text string }
		if json.Unmarshal(p.Update["content"], &content) == nil && content.Type == "text" && content.Text != "" {
			t.mu.Lock()
			item := "message-" + strconv.Itoa(t.commandCodeMessage)
			t.mu.Unlock()
			s.text(t, item, content.Text, false)
		}
	case "tool_call":
		id := str(p.Update, "toolCallId")
		if id == "" {
			return
		}
		// Text after a tool call is a new message: the turn's result is the
		// text that follows the last one.
		t.mu.Lock()
		t.commandCodeMessage++
		t.mu.Unlock()
		status := str(p.Update, "status")
		if status == "" {
			status = "pending"
		}
		s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: id, Tool: commandCodeToolName(p.Update), Status: status}, p.Update["rawInput"], ""))
	case "tool_call_update":
		id, status := str(p.Update, "toolCallId"), str(p.Update, "status")
		if id != "" && (status == "completed" || status == "failed") {
			s.emit(t, withImages(s.withToolPayload(Event{Kind: "tool_completed", ItemID: id, Status: status}, p.Update["rawInput"], grokToolOutput(p.Update)), grokToolImages(p.Update)))
		}
	case "usage_update":
		s.commandCodeContext(t, p.Update)
	}
}

// commandCodeToolKinds are the Agent Client Protocol's tool kinds.
var commandCodeToolKinds = map[string]bool{"read": true, "edit": true, "delete": true, "move": true, "search": true, "execute": true, "think": true, "fetch": true, "switch_mode": true, "other": true}

// commandCodeToolName is the tool's own identifier where its title is one,
// and otherwise its protocol kind. Command Code titles a known tool with a
// label and its path or command, which are never used as a name.
func commandCodeToolName(update map[string]json.RawMessage) string {
	if title := str(update, "title"); acpToolIdentifier.MatchString(title) {
		return title
	}
	if kind := str(update, "kind"); commandCodeToolKinds[kind] {
		return kind
	}
	return "tool"
}

// commandCodeContext reads Command Code's own figures for the session's
// context: used tokens against the model's window.
func (s *Session) commandCodeContext(t *Turn, update map[string]json.RawMessage) {
	var u struct {
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
	}
	if json.Unmarshal(mustMarshal(update), &u) != nil || u.Used == nil || *u.Used < 0 {
		return
	}
	s.mu.Lock()
	model := s.commandCodeModel
	s.mu.Unlock()
	used := *u.Used
	c := ContextSnapshot{Model: model, UsedTokens: &used}
	c.Observation = observation("usage_update", harness.Estimated)
	c.Reason = "Command Code's own count of the conversation's context"
	if u.Size != nil && *u.Size > 0 {
		size := *u.Size
		c.CapacityTokens = &size
		c.ModelCapacityTokens = cloneValue(&size)
	}
	setContextPercent(&c)
	s.observeContext(c, t)
}

// parseCommandCodeTurnUsage reads a turn's own accounting. Command Code sums
// each model response's totals, whose inputTokens already counts cached input
// (the cache figures are its details), so Input is taken as it is. A turn
// that reported nothing has no _meta.usage, and stays unknown.
func parseCommandCodeTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input  *int64 `json:"inputTokens"`
		Output *int64 `json:"outputTokens"`
		Read   *int64 `json:"cacheReadTokens"`
		Write  *int64 `json:"cacheWriteTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	read, write := valueOr(c.Read), valueOr(c.Write)
	input, output := *c.Input, *c.Output
	if input < 0 || output < 0 || read < 0 || write < 0 || read > input || write > input-read {
		return Usage{}
	}
	return Usage{Usage: harness.Usage{Known: true, Input: input, Output: output, CacheRead: read, CacheWrite: write, CacheKnown: c.Read != nil && c.Write != nil}}
}
