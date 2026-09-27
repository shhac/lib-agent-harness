package native

// Grok's `--output-format streaming-json` is an NDJSON view of its ACP
// session updates. Current builds have also emitted the update object directly,
// so the reader accepts both representations. The terminal `end` update is the
// only successful terminal marker; streamed prose is never itself a result.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type grokWireEvent struct {
	Type          string          `json:"type"`
	SessionUpdate string          `json:"sessionUpdate"`
	SessionID     string          `json:"sessionId"`
	Data          string          `json:"data"`
	Text          string          `json:"text"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Title         string          `json:"title"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Output        json.RawMessage `json:"output"`
	Usage         json.RawMessage `json:"usage"`
	StopReason    string          `json:"stopReason"`
	Message       string          `json:"message"`
	Error         json.RawMessage `json:"error"`
}

// grokUsage is deliberately a union of the snake-case headless and camel-case
// ACP spellings. The raw record is retained because Grok's resumed-session
// accounting scope is not yet documented.
type grokUsage struct {
	Input           *int `json:"input_tokens"`
	InputCamel      *int `json:"inputTokens"`
	Output          *int `json:"output_tokens"`
	OutputCamel     *int `json:"outputTokens"`
	CacheRead       *int `json:"cache_read_input_tokens"`
	CacheReadCamel  *int `json:"cacheReadInputTokens"`
	CacheWrite      *int `json:"cache_creation_input_tokens"`
	CacheWriteCamel *int `json:"cacheCreationInputTokens"`
	Reasoning       *int `json:"reasoning_tokens"`
	ReasoningCamel  *int `json:"reasoningTokens"`
}

func (u grokUsage) tokenUsage() TokenUsage {
	return TokenUsage{
		Input:      grokUsageValue(u.Input, u.InputCamel),
		Output:     grokUsageValue(u.Output, u.OutputCamel),
		CacheRead:  grokUsageValue(u.CacheRead, u.CacheReadCamel),
		CacheWrite: grokUsageValue(u.CacheWrite, u.CacheWriteCamel),
		Reasoning:  grokUsageValue(u.Reasoning, u.ReasoningCamel),
	}
}

func grokUsageValue(first, second *int) int {
	if first != nil {
		return *first
	}
	if second != nil {
		return *second
	}
	return 0
}

type grokTranscoder struct {
	markerSink

	structured bool
	completed  bool
	sawUsage   bool
	sessionID  string
	turns      int
	usage      TokenUsage
	rawUsage   []json.RawMessage
	report     json.RawMessage
	failure    string
	text       strings.Builder
	running    map[string]time.Time
	now        func() time.Time
}

func newGrokTranscoder(out io.Writer) *grokTranscoder {
	return &grokTranscoder{markerSink: markerSink{out: out}, running: map[string]time.Time{}, now: time.Now}
}

func (t *grokTranscoder) Write(p []byte) (int, error) { return t.writeLines(p, t.consume) }

func (t *grokTranscoder) beginTurn(prompt string) {
	t.completed = false
	t.sawUsage = false
	t.report = nil
	t.failure = ""
	t.text.Reset()
	t.userPrompt(prompt)
}

func (t *grokTranscoder) reachedTerminal() bool { return t.completed }

func (t *grokTranscoder) snapshot() Result {
	// Grok documents per-event usage but not whether a resumed invocation is a
	// session total. A first completed invocation is unambiguous; afterwards we
	// retain the latest observation and raw records without claiming a total.
	known := t.completed && t.sawUsage && t.turns == 1
	return Result{
		SessionID:  t.sessionID,
		Report:     append(json.RawMessage(nil), t.report...),
		Usage:      t.usage,
		RawUsage:   joinRawUsage(t.rawUsage),
		UsageKnown: known,
		Failure:    t.failure,
	}
}

func (t *grokTranscoder) Close() { t.flushPartial(t.consume) }

func (t *grokTranscoder) consume(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	ev, ok := decodeGrokWireEvent(line)
	if !ok {
		t.emit("", string(line))
		return
	}
	if ev.SessionID != "" && ev.SessionID != t.sessionID {
		t.sessionID = ev.SessionID
		t.event(Event{Kind: "session", SessionID: ev.SessionID})
		t.emit("", "session id: "+ev.SessionID)
	}

	kind := ev.Type
	if kind == "" {
		kind = ev.SessionUpdate
	}
	switch kind {
	case "agent_message_chunk", "text":
		t.renderText(grokEventText(ev))
	case "agent_thought_chunk", "thinking", "reasoning":
		text := grokEventText(ev)
		if text != "" {
			t.flushPrompt()
			t.event(Event{Kind: "reasoning", Text: text})
			t.emit("thinking", text)
		}
	case "tool_call":
		t.renderToolStart(ev)
	case "tool_call_update":
		t.renderToolUpdate(ev)
	case "error":
		t.renderFailure(grokEventError(ev))
	case "end":
		t.renderEnd(ev)
	}
}

func decodeGrokWireEvent(line []byte) (grokWireEvent, bool) {
	var outer struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &outer); err != nil {
		return grokWireEvent{}, false
	}
	if outer.Method == "session/update" {
		var params struct {
			SessionID string          `json:"sessionId"`
			Update    json.RawMessage `json:"update"`
		}
		if json.Unmarshal(outer.Params, &params) != nil || len(params.Update) == 0 {
			return grokWireEvent{}, false
		}
		var ev grokWireEvent
		if json.Unmarshal(params.Update, &ev) != nil {
			return grokWireEvent{}, false
		}
		if ev.SessionID == "" {
			ev.SessionID = params.SessionID
		}
		return ev, true
	}
	if len(bytes.TrimSpace(outer.Result)) > 0 && !bytes.Equal(bytes.TrimSpace(outer.Result), []byte("null")) {
		var ev grokWireEvent
		if json.Unmarshal(outer.Result, &ev) != nil {
			return grokWireEvent{}, false
		}
		if ev.Type == "" {
			ev.Type = "end"
		}
		return ev, true
	}
	var ev grokWireEvent
	if json.Unmarshal(line, &ev) != nil || (ev.Type == "" && ev.SessionUpdate == "") {
		return grokWireEvent{}, false
	}
	return ev, true
}

func (t *grokTranscoder) renderText(text string) {
	if text == "" {
		return
	}
	t.flushPrompt()
	t.text.WriteString(text)
	t.event(Event{Kind: "message", Text: text})
	t.emit("grok", text)
}

func (t *grokTranscoder) renderToolStart(ev grokWireEvent) {
	t.flushPrompt()
	name := ev.ToolName
	if name == "" {
		name = ev.Title
	}
	t.event(Event{Kind: "tool_start", ItemID: ev.ToolCallID, ToolName: name, Input: append(json.RawMessage(nil), ev.RawInput...)})
	if ev.ToolCallID != "" {
		t.running[ev.ToolCallID] = t.now()
	}
	t.emit("exec", grokToolDescription(ev))
}

func (t *grokTranscoder) renderToolUpdate(ev grokWireEvent) {
	output := grokEventOutput(ev)
	switch ev.Status {
	case "completed", "failed", "error", "cancelled":
		failed := ev.Status != "completed"
		t.event(Event{Kind: "tool_end", ItemID: ev.ToolCallID, Output: output, Failed: failed})
		elapsed := ""
		if started, ok := t.running[ev.ToolCallID]; ok {
			elapsed = " in " + t.now().Sub(started).Truncate(10*time.Millisecond).String()
			delete(t.running, ev.ToolCallID)
		}
		status := "succeeded"
		if failed {
			status = "failed"
		}
		_, _ = fmt.Fprintf(t.out, " %s%s:\n%s\n", status, elapsed, output)
	default:
		if output != "" {
			t.event(Event{Kind: "tool_output", ItemID: ev.ToolCallID, Output: output})
			t.emit("tool", output)
		}
	}
}

func (t *grokTranscoder) renderFailure(failure string) {
	if failure == "" {
		failure = "harness turn failed"
	}
	t.flushPrompt()
	t.failure = failure
	t.event(Event{Kind: "error", Text: failure})
	t.emit("error", failure)
}

func (t *grokTranscoder) renderEnd(ev grokWireEvent) {
	t.flushPrompt()
	t.completed = true
	t.turns++
	if text := grokEventText(ev); text != "" && t.text.Len() == 0 {
		t.renderText(text)
	}
	if t.text.Len() > 0 {
		if t.structured {
			t.report = json.RawMessage(t.text.String())
		} else {
			t.report, _ = json.Marshal(t.text.String())
		}
	}
	if len(ev.Usage) > 0 && !bytes.Equal(bytes.TrimSpace(ev.Usage), []byte("null")) {
		var usage grokUsage
		if json.Unmarshal(ev.Usage, &usage) == nil {
			t.usage = usage.tokenUsage()
			t.sawUsage = true
			t.rawUsage = append(t.rawUsage, append(json.RawMessage(nil), ev.Usage...))
			t.event(Event{Kind: "usage", Usage: t.usage, UsageKnown: t.turns == 1})
		}
	}
	if ev.Status == "failed" || ev.Status == "error" || ev.StopReason == "error" {
		t.renderFailure(grokEventError(ev))
	}
}

func grokEventText(ev grokWireEvent) string {
	if ev.Data != "" {
		return ev.Data
	}
	if ev.Text != "" {
		return ev.Text
	}
	if len(ev.Content) == 0 {
		return ""
	}
	var content struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(ev.Content, &content) == nil && content.Text != "" {
		return content.Text
	}
	var text string
	if json.Unmarshal(ev.Content, &text) == nil {
		return text
	}
	return ""
}

func grokEventOutput(ev grokWireEvent) string {
	for _, value := range []json.RawMessage{ev.RawOutput, ev.Output, ev.Content} {
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			return text
		}
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(value, &content) == nil && content.Text != "" {
			return content.Text
		}
		return string(value)
	}
	return ""
}

func grokEventError(ev grokWireEvent) string {
	if ev.Message != "" {
		return ev.Message
	}
	if len(ev.Error) > 0 {
		return decodeResultText(ev.Error)
	}
	return ""
}

func grokToolDescription(ev grokWireEvent) string {
	name := ev.ToolName
	if name == "" {
		name = ev.Title
	}
	if len(ev.RawInput) == 0 || bytes.Equal(bytes.TrimSpace(ev.RawInput), []byte("null")) {
		return name
	}
	return name + "\n" + string(ev.RawInput)
}
