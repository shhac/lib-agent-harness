package session

import (
	"encoding/json"
	"strconv"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// The session/update notifications every ACP agent sends while a turn runs:
// streamed message text, tool calls and their updates, and the context the
// session occupies. Which text item a chunk belongs to, how a tool is named
// and what else an update can carry are the engine's own.

// acpUpdates is one ACP engine's reading of session/update.
type acpUpdates struct {
	// messageItem names the text item a message chunk belongs to.
	messageItem func(t *Turn, update map[string]json.RawMessage) string
	// newMessageAfterTool starts a new text item after each tool call, so the
	// turn's result is the text that follows the last one.
	newMessageAfterTool bool
	// toolName is the tool's own identifier, never its title, which carries
	// paths and commands; "" names it "tool".
	toolName func(update map[string]json.RawMessage) string
	// context reads a usage_update into the session's context, if the engine
	// states one.
	context func(s *Session, t *Turn, update map[string]json.RawMessage)
}

// acpSessionUpdate reads a notification's params as an update for the
// session ref names, and false for one about any other session.
func acpSessionUpdate(params json.RawMessage, ref Ref) (string, map[string]json.RawMessage, bool) {
	var p struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil || p.Update == nil || ref.ID == "" || p.SessionID != ref.ID {
		return "", nil, false
	}
	return str(p.Update, "sessionUpdate"), p.Update, true
}

// acpUpdate publishes one session/update for the active turn.
func (s *Session) acpUpdate(t *Turn, ref Ref, params json.RawMessage, u acpUpdates) {
	kind, update, ok := acpSessionUpdate(params, ref)
	if !ok {
		return
	}
	switch kind {
	case "agent_message_chunk":
		var content struct{ Type, Text string }
		if json.Unmarshal(update["content"], &content) == nil && content.Type == "text" && content.Text != "" {
			s.text(t, u.messageItem(t, update), content.Text, false)
		}
	case "tool_call":
		id := str(update, "toolCallId")
		if id == "" {
			return
		}
		if u.newMessageAfterTool {
			t.mu.Lock()
			t.acpMessage++
			t.mu.Unlock()
		}
		status := str(update, "status")
		if status == "" {
			status = "pending"
		}
		name := u.toolName(update)
		if name == "" {
			name = "tool"
		}
		s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: id, Tool: name, Status: status}, update["rawInput"], ""))
	case "tool_call_update":
		id, status := str(update, "toolCallId"), str(update, "status")
		if id != "" && (status == "completed" || status == "failed") {
			s.emit(t, withImages(s.withToolPayload(Event{Kind: "tool_completed", ItemID: id, Status: status}, update["rawInput"], acpToolOutput(update)), acpToolImages(update)))
		}
	case "usage_update":
		if u.context != nil {
			u.context(s, t, update)
		}
	}
}

// acpCountedItem names a turn's text items by prefix and the turn's message
// count.
func acpCountedItem(prefix string) func(t *Turn, update map[string]json.RawMessage) string {
	return func(t *Turn, _ map[string]json.RawMessage) string {
		t.mu.Lock()
		defer t.mu.Unlock()
		return prefix + strconv.Itoa(t.acpMessage)
	}
}

// acpContext publishes the context an agent states in a usage_update: the
// tokens the conversation occupies, and the window it occupies them in, at
// the quality the engine's figures have.
func (s *Session) acpContext(t *Turn, update map[string]json.RawMessage, quality harness.Measurement, reason string) {
	var u struct {
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
	}
	if json.Unmarshal(mustMarshal(update), &u) != nil || u.Used == nil || *u.Used < 0 {
		return
	}
	s.mu.Lock()
	model := s.acpModel
	s.mu.Unlock()
	used := *u.Used
	c := ContextSnapshot{Model: model, UsedTokens: &used}
	c.Observation = observation("usage_update", quality)
	c.Reason = reason
	if u.Size != nil && *u.Size > 0 {
		size := *u.Size
		c.CapacityTokens = &size
		c.ModelCapacityTokens = cloneValue(&size)
	}
	setContextPercent(&c)
	s.observeContext(c, t)
}

// acpToolImages are the image blocks among an ACP tool call's content, which
// wraps each block: {"type":"content","content":{"type":"image",...}}.
func acpToolImages(update map[string]json.RawMessage) toolImages {
	var images toolImages
	var blocks []struct {
		Type    string           `json:"type"`
		Content toolContentBlock `json:"content"`
	}
	if json.Unmarshal(update["content"], &blocks) != nil {
		return images
	}
	for _, b := range blocks {
		if b.Type == "content" && b.Content.Type == "image" {
			images.add(b.Content)
		}
	}
	return images
}

// acpToolOutput is an ACP tool call's result: its rawOutput, as text when it
// is a string and as JSON text otherwise, or failing that the text of its
// content blocks. Image blocks are reported as images; another block with no
// text, such as a diff, keeps the content's JSON text rather than dropping it.
func acpToolOutput(update map[string]json.RawMessage) string {
	if raw := update["rawOutput"]; !rawjson.Absent(raw) {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
		return string(raw)
	}
	var blocks []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	raw := update["content"]
	if rawjson.Absent(raw) || json.Unmarshal(raw, &blocks) != nil {
		return toolResultText(raw)
	}
	if len(blocks) == 0 {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Type == "content" && b.Content.Type == "image":
			// Reported as an image by acpToolImages.
		case b.Type == "content" && b.Content.Type == "text":
			parts = append(parts, b.Content.Text)
		default:
			return string(raw)
		}
	}
	return strings.Join(parts, "\n")
}
