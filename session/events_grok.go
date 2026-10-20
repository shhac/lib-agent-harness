package session

import (
	"encoding/json"
	"strconv"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// Grok's session/update dialect, and the prompt response that ends a turn.
// Kept apart from the other engines because its usage arrives in two scopes
// that must not be confused: each model response's figures stream as a
// response_completed notification, and the turn's own accounting is in the
// prompt's response. Every shape here was observed from grok 1.0.41.

// grokPermissionRejected is the turn failure code for a cancellation Grok says
// was a refused permission, beside the shared ACP codes in acp_turn.go and
// the refusal codes in grok.go.
const grokPermissionRejected = "permission_rejected"

// grokEvent routes one notification for the active turn. Streamed content is
// ACP's session/update; per-response usage is Grok's own extension envelope.
func (s *Session) grokEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	method := str(m, "method")
	if method != "session/update" && method != "_x.ai/session_notification" {
		return
	}
	var p struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(m["params"], &p) != nil || p.Update == nil || ref.ID == "" || p.SessionID != ref.ID {
		return
	}
	kind := str(p.Update, "sessionUpdate")
	if method == "_x.ai/session_notification" {
		if kind == "response_completed" {
			s.grokResponseCompleted(t, p.Update)
		}
		return
	}
	switch kind {
	case "agent_message_chunk":
		var content struct{ Type, Text string }
		if json.Unmarshal(p.Update["content"], &content) == nil && content.Type == "text" && content.Text != "" {
			t.mu.Lock()
			item := "response-" + strconv.Itoa(t.grokResponse)
			t.mu.Unlock()
			s.text(t, item, content.Text, false)
		}
	case "tool_call":
		id := str(p.Update, "toolCallId")
		if id == "" {
			return
		}
		status := str(p.Update, "status")
		if status == "" {
			status = "pending"
		}
		s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: id, Tool: grokToolName(p.Update), Status: status}, p.Update["rawInput"], ""))
	case "tool_call_update":
		id, status := str(p.Update, "toolCallId"), str(p.Update, "status")
		if id != "" && (status == "completed" || status == "failed") {
			s.emit(t, withImages(s.withToolPayload(Event{Kind: "tool_completed", ItemID: id, Status: status}, p.Update["rawInput"], grokToolOutput(p.Update)), grokToolImages(p.Update)))
		}
	}
}

// grokToolImages are the image blocks among an ACP tool call's content, which
// wraps each block: {"type":"content","content":{"type":"image",...}}.
func grokToolImages(update map[string]json.RawMessage) toolImages {
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

// grokToolOutput is an ACP tool call's result: its rawOutput, as text when it
// is a string and as JSON text otherwise, or failing that the text of its
// content blocks. Image blocks are reported as images; another block with no
// text, such as a diff, keeps the content's JSON text rather than dropping it.
func grokToolOutput(update map[string]json.RawMessage) string {
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
			// Reported as an image by grokToolImages.
		case b.Type == "content" && b.Content.Type == "text":
			parts = append(parts, b.Content.Text)
		default:
			return string(raw)
		}
	}
	return strings.Join(parts, "\n")
}

// grokResponseCompleted publishes one model response's usage while the turn
// runs, estimates the context from it, and closes that response's text.
func (s *Session) grokResponseCompleted(t *Turn, update map[string]json.RawMessage) {
	usage := parseGrokResponseUsage(update["usage"])
	s.observeRequestUsage(t, usage)
	s.grokContext(t, usage)
	t.mu.Lock()
	t.grokResponse++
	t.mu.Unlock()
}

// grokContext estimates occupancy from the latest response's whole input, and
// states capacity from the window Grok's model state gives for the session's
// model. Pending output and tool results are not counted, hence Estimated.
func (s *Session) grokContext(t *Turn, u Usage) {
	if !u.Known {
		return
	}
	s.mu.Lock()
	model, capacity := s.grokModel, s.grokCapacity
	s.mu.Unlock()
	used := u.Input
	c := ContextSnapshot{Model: model, UsedTokens: &used}
	c.Observation = observation("response_completed.usage", harness.Estimated)
	c.Reason = "latest model input; pending output and tool results are not counted"
	if capacity > 0 {
		c.CapacityTokens = &capacity
		c.ModelCapacityTokens = cloneValue(&capacity)
	}
	setContextPercent(&c)
	s.observeContext(c, t)
}

// grokToolName is the tool's own identifier, never its title, which carries
// paths and commands.
func grokToolName(update map[string]json.RawMessage) string {
	var meta struct {
		Tool struct {
			Name string `json:"name"`
		} `json:"x.ai/tool"`
	}
	_ = json.Unmarshal(update["_meta"], &meta)
	for _, name := range []string{meta.Tool.Name, str(update, "title")} {
		if acpToolIdentifier.MatchString(name) {
			return name
		}
	}
	return "tool"
}

// readGrokPromptEnd reads Grok's prompt response, which states the turn's
// usage and why a cancellation happened in its _meta.
func readGrokPromptEnd(body json.RawMessage, interruptRequested bool) (acpTurnEnd, bool) {
	var r struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Usage    json.RawMessage `json:"usage"`
			Category string          `json:"cancellationCategory"`
		} `json:"_meta"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		return acpTurnEnd{}, false
	}
	status, code := grokOutcome(r.StopReason, r.Meta.Category, interruptRequested)
	return acpTurnEnd{status: status, code: code, usage: parseGrokTurnUsage(r.Meta.Usage)}, true
}

// grokOutcome is the shared outcome, except that a cancellation nobody asked
// for which Grok says was a refused permission is reported as one.
func grokOutcome(stop, category string, interruptRequested bool) (string, string) {
	if stop == "cancelled" && !interruptRequested && category == "PermissionRejected" {
		return "failed", grokPermissionRejected
	}
	return acpOutcome(stop, interruptRequested)
}

// parseGrokTurnUsage reads the turn's accounting from a prompt response. Its
// inputTokens already counts cached input: across every observed turn it
// equals the sum of that turn's responses' uncached input and cache reads.
func parseGrokTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Read      *int64 `json:"cachedReadTokens"`
		Write     *int64 `json:"cacheCreationTokens"`
		Reasoning *int64 `json:"reasoningTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	read, write, reasoning := valueOr(c.Read), valueOr(c.Write), valueOr(c.Reasoning)
	input, output := *c.Input, *c.Output
	if input < 0 || output < 0 || read < 0 || write < 0 || reasoning < 0 || read > input || write > input-read || reasoning > output {
		return Usage{}
	}
	return Usage{Usage: harness.Usage{Known: true, Input: input, Output: output, CacheRead: read, CacheWrite: write, Reasoning: reasoning, CacheKnown: c.Read != nil && c.Write != nil, ReasoningKnown: c.Reasoning != nil}}
}

// parseGrokResponseUsage reads one response's figures, whose input_tokens
// excludes both cache figures, into the shared shape, whose Input includes
// them.
func parseGrokResponseUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"input_tokens"`
		Output    *int64 `json:"output_tokens"`
		Read      *int64 `json:"cache_read_input_tokens"`
		Write     *int64 `json:"cache_creation_input_tokens"`
		Reasoning *int64 `json:"reasoning_tokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	read, write, reasoning := valueOr(c.Read), valueOr(c.Write), valueOr(c.Reasoning)
	input, output := *c.Input, *c.Output
	if input < 0 || output < 0 || read < 0 || write < 0 || reasoning < 0 || reasoning > output || input > maxInt64-read || input+read > maxInt64-write {
		return Usage{}
	}
	return Usage{Usage: harness.Usage{Known: true, Input: input + read + write, Output: output, CacheRead: read, CacheWrite: write, Reasoning: reasoning, CacheKnown: c.Read != nil && c.Write != nil, ReasoningKnown: c.Reasoning != nil}}
}

func valueOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
