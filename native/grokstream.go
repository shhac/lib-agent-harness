package native

// Grok's `--output-format streaming-json` is NDJSON, one `type`-tagged object
// per line, derived from its ACP session updates (verified against grok
// 1.0.41: available_commands, thought, text, tool_call, tool_call_update,
// usage, end). ACP `session/update` envelopes are unwrapped too, since the
// leaf shapes are the same. The terminal `end` event is the only successful
// terminal marker; streamed prose is never itself a result.

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
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
	StopReason    string          `json:"stopReason"`
	Message       string          `json:"message"`
	Error         json.RawMessage `json:"error"`

	// Spend fields, on `end` (and on `error` when usage was recorded).
	Usage            *grokUsage      `json:"usage"`
	UsageIncomplete  bool            `json:"usage_is_incomplete"`
	TotalCostUSD     *float64        `json:"total_cost_usd"`
	TotalCostTicks   *int64          `json:"total_cost_usd_ticks"`
	CostPartial      bool            `json:"cost_is_partial"`
	StructuredOutput json.RawMessage `json:"structuredOutput"`
}

// grokUsage is the headless spend projection. Its input_tokens is uncached
// input only, so the shared Input, which counts every prompt token, adds both
// cache figures back. ACP's camel-case inputTokens is not read: the headless
// fields are the ones that split the cache out.
type grokUsage struct {
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  int64  `json:"cache_read_input_tokens"`
	CacheWrite int64  `json:"cache_creation_input_tokens"`
	Reasoning  *int64 `json:"reasoning_tokens"`
}

func (u grokUsage) usage() harness.Usage {
	return harness.Usage{
		Input:          u.Input + u.CacheRead + u.CacheWrite,
		Output:         u.Output,
		CacheRead:      u.CacheRead,
		CacheWrite:     u.CacheWrite,
		Reasoning:      valueOrZero(u.Reasoning),
		CacheKnown:     true,
		ReasoningKnown: u.Reasoning != nil,
	}
}

type grokTranscoder struct {
	markerSink

	structured      bool
	completed       bool
	sawUsage        bool
	sawCost         bool
	usageIncomplete bool
	costIncomplete  bool
	sessionID       string
	usage           harness.Usage // summed across every invocation
	costTicks       int64         // exact integer cost, 1 USD = 10^10 ticks
	rawUsage        []json.RawMessage
	report          json.RawMessage
	failure         string
	// text is the prose of the model response in progress. A per-response
	// `usage` line closes it into lastResponse, so narration before a tool call
	// is never the report, and a final response without prose leaves none.
	text         strings.Builder
	lastResponse string
	// errorSpend is spend an `error` event carried. It counts only if `end`
	// reports none, since `end` would otherwise report the same spend again.
	errorSpend *grokSpend
	running    map[string]time.Time
	now        func() time.Time
}

type grokSpend struct {
	ev   grokWireEvent
	line []byte
}

const grokCostTicksPerUSD = 1e10

func newGrokTranscoder(out io.Writer) *grokTranscoder {
	return &grokTranscoder{markerSink: markerSink{out: out}, running: map[string]time.Time{}, now: time.Now}
}

func (t *grokTranscoder) Write(p []byte) (int, error) { return t.writeLines(p, t.consume) }

// beginTurn clears the previous report and failure. Usage and cost flags
// survive, as for Claude: they describe the summed session, not this turn.
func (t *grokTranscoder) beginTurn(prompt string) {
	t.completed = false
	t.report = nil
	t.failure = ""
	t.text.Reset()
	t.lastResponse = ""
	t.errorSpend = nil
	t.userPrompt(prompt)
}

func (t *grokTranscoder) reachedTerminal() bool { return t.completed }

func (t *grokTranscoder) failureCause() (harness.Cause, *time.Time) { return "", nil }

func (t *grokTranscoder) snapshot() Result {
	usage := t.usage
	usage.Known = t.completed && t.sawUsage && !t.usageIncomplete
	return Result{
		SessionID: t.sessionID,
		Report:    append(json.RawMessage(nil), t.report...),
		Usage:     usage,
		Cost:      harness.Cost{USD: float64(t.costTicks) / grokCostTicksPerUSD, Known: t.completed && t.sawCost && !t.costIncomplete},
		RawUsage:  joinRawUsage(t.rawUsage),
		Failure:   t.failure,
	}
}

// Close flushes a trailing line. An invocation that never reached `end` has
// unaccounted spend no later invocation can repair.
func (t *grokTranscoder) Close() {
	t.flushPartial(t.consume)
	if t.completed {
		return
	}
	if t.errorSpend != nil {
		t.recordSpend(*t.errorSpend)
		t.errorSpend = nil
	}
	t.usageIncomplete = true
	t.costIncomplete = true
}

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

	switch ev.Type {
	case "agent_message_chunk", "text":
		t.renderText(grokEventText(ev))
	case "agent_thought_chunk", "thought":
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
	case "usage":
		t.lastResponse = t.text.String()
		t.text.Reset()
	case "error":
		if ev.hasSpend() {
			t.errorSpend = &grokSpend{ev: ev, line: append([]byte(nil), line...)}
		}
		t.renderFailure(grokEventError(ev))
	case "end":
		t.renderEnd(ev, line)
	}
}

// decodeGrokWireEvent reads a type-tagged line or an ACP session/update
// notification. Any other JSON-RPC message, such as a response, is not a
// stream event: treating a response as `end` would report success for a turn
// that never finished.
func decodeGrokWireEvent(line []byte) (grokWireEvent, bool) {
	var outer struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
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
		return ev.normalized()
	}
	var ev grokWireEvent
	if json.Unmarshal(line, &ev) != nil {
		return grokWireEvent{}, false
	}
	return ev.normalized()
}

// normalized files an ACP update under Type, the one field consume reads.
func (ev grokWireEvent) normalized() (grokWireEvent, bool) {
	if ev.Type == "" {
		ev.Type = ev.SessionUpdate
	}
	return ev, ev.Type != ""
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
	t.event(Event{Kind: "tool_start", ItemID: ev.ToolCallID, ToolName: ev.toolName(), Input: append(json.RawMessage(nil), ev.RawInput...)})
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
		var elapsed time.Duration
		started, timed := t.running[ev.ToolCallID]
		if timed {
			elapsed = t.now().Sub(started)
			delete(t.running, ev.ToolCallID)
		}
		t.toolEnded(failed, elapsed, timed, output)
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

func (t *grokTranscoder) renderEnd(ev grokWireEvent, line []byte) {
	t.flushPrompt()
	if text := grokEventText(ev); text != "" && t.finalText() == "" {
		t.renderText(text)
	}
	if failure := t.recordEnd(ev, line); failure != "" {
		t.renderFailure(failure)
	}
	t.writeSpendTrailer()
}

// recordEnd folds the terminal event into the transcoder's state and reports
// why the turn failed, if it did; nothing is written. A failed turn keeps no
// report, so partial prose can never be read back as an answer.
func (t *grokTranscoder) recordEnd(ev grokWireEvent, line []byte) string {
	t.completed = true
	t.settleSpend(ev, line)
	report := t.endReport(ev)
	failure := grokEndFailure(ev, t.structured, len(report) > 0)
	if failure == "" && t.failure == "" {
		t.report = report
	}
	return failure
}

func (t *grokTranscoder) endReport(ev grokWireEvent) json.RawMessage {
	if t.structured {
		if rawjson.Absent(ev.StructuredOutput) {
			return nil
		}
		return append(json.RawMessage(nil), ev.StructuredOutput...)
	}
	text := t.finalText()
	if text == "" {
		return nil
	}
	report, _ := json.Marshal(text)
	return report
}

// finalText is the last response's prose: the one in progress if it has any,
// or else the last one a `usage` line closed.
func (t *grokTranscoder) finalText() string {
	if t.text.Len() > 0 {
		return t.text.String()
	}
	return t.lastResponse
}

// settleSpend records the invocation's spend once: from `end`, or from an
// earlier `error` when `end` carries none. End's incompleteness flags hold
// either way, since they describe the whole invocation.
func (t *grokTranscoder) settleSpend(ev grokWireEvent, line []byte) {
	spend := grokSpend{ev: ev, line: line}
	if !ev.hasSpend() && t.errorSpend != nil {
		spend = *t.errorSpend
	}
	t.errorSpend = nil
	t.recordSpend(spend)
	if ev.UsageIncomplete {
		t.usageIncomplete = true
	}
	if ev.UsageIncomplete || ev.CostPartial {
		t.costIncomplete = true
	}
}

// recordSpend folds one invocation's usage and cost in. Grok omits spend
// fields when a prompt never reached the model, marks partial figures, and
// omits cost it could not fully account; none of those is evidence of zero.
func (t *grokTranscoder) recordSpend(spend grokSpend) {
	ev := spend.ev
	if ev.Usage != nil {
		raw := extractUsage(spend.line)
		if raw == nil {
			raw, _ = json.Marshal(ev.Usage)
		}
		t.rawUsage = append(t.rawUsage, raw)
		t.usage = addUsage(t.usage, ev.Usage.usage(), !t.sawUsage)
		t.sawUsage = true
	}
	if ev.Usage == nil || ev.UsageIncomplete {
		t.usageIncomplete = true
	}
	switch {
	case ev.TotalCostTicks != nil:
		t.costTicks += *ev.TotalCostTicks
		t.sawCost = true
	case ev.TotalCostUSD != nil:
		t.costTicks += int64(math.Round(*ev.TotalCostUSD * grokCostTicksPerUSD))
		t.sawCost = true
	}
	if (ev.TotalCostUSD == nil && ev.TotalCostTicks == nil) || ev.CostPartial || ev.UsageIncomplete {
		t.costIncomplete = true
	}
}

func (ev grokWireEvent) hasSpend() bool {
	return ev.Usage != nil || ev.TotalCostUSD != nil || ev.TotalCostTicks != nil
}

func (t *grokTranscoder) writeSpendTrailer() {
	usage := t.usage
	usage.Known = t.sawUsage && !t.usageIncomplete
	cost := harness.Cost{USD: float64(t.costTicks) / grokCostTicksPerUSD, Known: t.sawCost && !t.costIncomplete}
	t.event(Event{Kind: "usage", Usage: usage, Cost: cost})
	t.spendTrailer(usage, t.sawUsage, cost)
}

// grokEndFailure reads the turn's stop reason. Only end_turn is a finished
// turn; max_tokens, max_turn_requests, refusal, cancelled and any reason this
// parser does not know are failures, never a report.
func grokEndFailure(ev grokWireEvent, structured, hasReport bool) string {
	if ev.Status == "failed" || ev.Status == "error" || ev.StopReason == "error" {
		if message := grokEventError(ev); message != "" {
			return message
		}
		return "the run reported an error with no detail"
	}
	if ev.StopReason != "" && ev.StopReason != "end_turn" {
		return "the run stopped: " + ev.StopReason
	}
	if structured && !hasReport {
		return "the run ended without structured output"
	}
	return ""
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
		if rawjson.Absent(value) {
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

func (ev grokWireEvent) toolName() string {
	if ev.ToolName != "" {
		return ev.ToolName
	}
	return ev.Title
}

func grokToolDescription(ev grokWireEvent) string {
	if rawjson.Absent(ev.RawInput) {
		return ev.toolName()
	}
	return ev.toolName() + "\n" + string(ev.RawInput)
}

func valueOrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
