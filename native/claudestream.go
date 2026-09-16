package native

// claude -p emits newline-delimited JSON, not a readable transcript. This file
// converts one into the other as the run streams: every event is rendered into
// the same bare-marker format `codex exec` prints, so agent.log stays a single
// cross-engine contract and the dashboard's one parser serves both drivers
// (see internal/dashboard/ui/src/lib/agentlog.ts). The transcoder doubles as
// the run's state accumulator, since the same pass already sees the session
// id, the token usage, and the final structured report.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// streamEvent is the subset of claude's stream-json protocol this driver
// reads. Unknown types and fields are ignored by design: the protocol grows,
// and an unrecognised event must degrade to "nothing rendered", never to a
// failed review.
type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     *float64        `json:"total_cost_usd"`
	// IsError and Result carry the run's own account of a failure. Without
	// them a failed run rendered as a bare "tokens used 0" and the reason was
	// discarded, which is precisely the transcript you need most.
	IsError bool            `json:"is_error"`
	Result  json.RawMessage `json:"result"`
	Usage   *streamUsage    `json:"usage"`
}

// streamUsage is claude's usage report for one invocation. It carries more
// structure than these four fields (5m/1h cache-write tiers, server tool
// calls, per-message iterations); that detail is preserved verbatim in
// rawUsage rather than modelled here, so a pricing question about a tier we
// never mapped stays a query instead of a migration.
type streamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// addUsage folds one invocation's usage into the run's running total.
//
// Added rather than replaced because a resumed run reports only its OWN turn,
// not the session's total (verified live: a resume whose turn produced 43
// output tokens reported 43, not the 4,529 the session had accumulated). Same
// conclusion as codex's per-invocation trailers.
//
// Summing is correct for claude specifically and wrong for codex, whose
// turn.completed reports the session total every time; see
// codexTranscoder.recordUsage.
func addUsage(acc TokenUsage, u streamUsage) TokenUsage {
	acc.Input += u.InputTokens
	acc.Output += u.OutputTokens
	acc.CacheWrite += u.CacheCreationInputTokens
	acc.CacheRead += u.CacheReadInputTokens
	return acc
}

// contentBlock is one item of an assistant or user message's content array.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`   // tool_use blocks
	Name      string          `json:"name"` // tool_use blocks
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"` // tool_result blocks, referring back to an ID
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// structuredOutputTool is how --json-schema is actually delivered: the agent
// reports by calling this tool rather than by writing a final message. The
// call and its acknowledgement are plumbing, not review activity, so they are
// kept out of the transcript; renderResult prints the report itself instead.
const structuredOutputTool = "StructuredOutput"

// streamTranscoder consumes claude's stream-json stdout and writes the marker
// transcript into out, accumulating the state the resume policy reads back.
// It implements io.Writer so the subprocess seam matches codex's: production
// hands it the process stdout, tests write a canned stream into it directly.
type streamTranscoder struct {
	markerSink // line reassembly, marker writing, the prompt banner

	structured      bool
	completed       bool
	sawUsage        bool
	sawCost         bool
	usageIncomplete bool
	costIncomplete  bool
	sessionID       string
	usage           TokenUsage        // summed across every invocation
	rawUsage        []json.RawMessage // every result event's usage, verbatim
	costUSD         float64           // ditto; see renderResult for what this figure means
	report          json.RawMessage   // structured_output of the most recent result message
	failure         string            // the run's own account of why it ended without a report
	promptSeen      bool              // the first text-bearing user message is the prompt, the rest are tool results
	pending         []time.Time       // start times of tool calls awaiting a result, FIFO
	suppressed      map[string]bool   // tool_use ids whose result must be dropped too

	now func() time.Time // injectable clock so the rendered durations are testable
}

func newStreamTranscoder(out io.Writer) *streamTranscoder {
	return &streamTranscoder{markerSink: markerSink{out: out}, suppressed: map[string]bool{}, now: time.Now}
}

// userPrompt overrides the embedded registration to also record that the
// prompt has been seen. That flag is claude-specific: its stream replays the
// prompt as a `user` message, so without it the driver-supplied text and the
// stream's echo of the same text would both render.
func (t *streamTranscoder) userPrompt(text string) {
	t.promptSeen = true
	t.markerSink.userPrompt(text)
}

func (t *streamTranscoder) Write(p []byte) (int, error) { return t.writeLines(p, t.consume) }

// Close renders any trailing line the stream ended without a newline on, plus
// a prompt no event ever arrived to flush (a run that died before its first
// message still shows what it was asked). No token trailer here, unlike
// codex's: claude prints one per result event, from renderResult.
func (t *streamTranscoder) Close() {
	t.flushPartial(t.consume)
	// An interrupted process may never emit its result. No future per-invocation
	// total can repair the missing turn, so session accounting remains partial.
	if !t.completed {
		t.usageIncomplete = true
		t.costIncomplete = true
	}
}

// consume renders one stream line. A line that isn't a JSON event is passed
// through verbatim: claude occasionally prints plain warnings, and dropping
// them would lose diagnostics the raw view is meant to preserve.
func (t *streamTranscoder) consume(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.emit("", string(line))
		return
	}
	if ev.SessionID != "" {
		t.sessionID = ev.SessionID
	}
	if ev.Type != "system" {
		// Everything but the banner counts as the run getting underway, so
		// the prompt goes in just above it.
		t.flushPrompt()
	}
	switch ev.Type {
	case "system":
		t.renderSystem(ev)
	case "assistant":
		t.renderBlocks(ev, true)
	case "user":
		t.renderBlocks(ev, false)
	case "result":
		t.renderResult(ev, line)
	}
}

func (t *streamTranscoder) renderSystem(ev streamEvent) {
	// The init event is the run banner. codex prints "session id: <uuid>"
	// there and the UI parser treats everything before the first marker as
	// banner text, so the same line lands in the same place.
	if ev.Subtype == "init" && ev.SessionID != "" {
		t.event(Event{Kind: "session", SessionID: ev.SessionID})
		t.emit("", "session id: "+ev.SessionID)
	}
}

// renderBlocks walks a message's content array. Assistant messages carry
// thinking, prose, and tool calls; user messages carry the prompt (once) and
// tool results.
func (t *streamTranscoder) renderBlocks(ev streamEvent, assistant bool) {
	if ev.Message == nil {
		return
	}
	for _, b := range decodeContent(ev.Message.Content) {
		switch {
		case b.Type == "thinking" && b.Thinking != "":
			t.event(Event{Kind: "reasoning", Text: b.Thinking})
			t.emit("thinking", b.Thinking)
		case b.Type == "text" && assistant && b.Text != "":
			t.event(Event{Kind: "message", Text: b.Text})
			t.emit("claude", b.Text)
		case b.Type == "text" && !assistant && !t.promptSeen && b.Text != "":
			t.promptSeen = true
			t.emit("user", b.Text)
		case b.Type == "tool_use" && b.Name == structuredOutputTool:
			t.suppressed[b.ID] = true
		case b.Type == "tool_use":
			t.event(Event{Kind: "tool_start", ItemID: b.ID, ToolName: b.Name, Input: append(json.RawMessage(nil), b.Input...)})
			t.pending = append(t.pending, t.now())
			t.emit("exec", renderToolUse(b))
		case b.Type == "tool_result" && t.suppressed[b.ToolUseID]:
			delete(t.suppressed, b.ToolUseID)
		case b.Type == "tool_result":
			t.emitToolResult(b)
		}
	}
}

// emitToolResult closes the oldest unfinished tool call. Results carry a
// tool_use_id, but the marker format has no ids and the stream cannot be
// reordered, so pairing is FIFO by arrival, exactly the best-effort contract
// the UI parser already documents for codex's interleaved sections.
func (t *streamTranscoder) emitToolResult(b contentBlock) {
	t.event(Event{Kind: "tool_end", ItemID: b.ToolUseID, Output: decodeToolResult(b.Content), Failed: b.IsError})
	status := "succeeded"
	if b.IsError {
		status = "failed"
	}
	elapsed := ""
	if len(t.pending) > 0 {
		elapsed = " in " + t.now().Sub(t.pending[0]).Truncate(10*time.Millisecond).String()
		t.pending = t.pending[1:]
	}
	_, _ = fmt.Fprintf(t.out, " %s%s:\n%s\n", status, elapsed, decodeToolResult(b.Content))
}

func (t *streamTranscoder) renderResult(ev streamEvent, rawLine []byte) {
	t.completed = true
	if len(ev.StructuredOutput) > 0 && !bytes.Equal(bytes.TrimSpace(ev.StructuredOutput), []byte("null")) {
		t.report = ev.StructuredOutput
		// The report is the run's conclusion, and with --json-schema it
		// arrives as a suppressed tool call rather than a message, so nothing
		// else would render it. Emitting it under the agent marker is what
		// gives the dashboard its decision bubble, exactly as codex's final
		// assistant message does.
		t.emit("claude", jsonCompact(string(t.report)))
	}
	// Claude can emit a terminal error with all-zero counters despite having
	// streamed output. Such placeholders are not evidence of a free invocation.
	failed := ev.IsError || strings.HasPrefix(ev.Subtype, "error")
	usageUsable := ev.Usage != nil && (!failed || addUsage(TokenUsage{}, *ev.Usage).Total() > 0)
	if ev.Usage != nil {
		if raw := extractUsage(rawLine); raw != nil {
			t.rawUsage = append(t.rawUsage, raw)
		}
	}
	if usageUsable {
		t.sawUsage = true
		t.usage = addUsage(t.usage, *ev.Usage)
	} else {
		t.usageIncomplete = true
	}

	// A run that ends with no report has failed; say so in the transcript
	// rather than leaving a bare token trailer. The CLI's own wording is the
	// best diagnosis available, and it is otherwise dropped.
	if !t.structured && !ev.IsError && len(t.report) == 0 {
		t.report = ev.Result
	}
	if reason := resultFailure(ev); reason != "" && (t.structured || ev.IsError) {
		t.failure = reason
		t.event(Event{Kind: "error", Text: reason})
		t.emit("error", reason)
	}

	// Summed for the same reason as the tokens, and verified the same way.
	// NOT money charged: on a subscription this is what the run's tokens
	// would have cost at API rates, which is precisely the figure
	// --max-budget-usd is compared against, and the only per-review spend
	// signal either engine reports.
	// On error, unusable usage also makes a carried-over cost untrustworthy.
	costUsable := ev.TotalCostUSD != nil && *ev.TotalCostUSD >= 0 && (!failed || (usageUsable && *ev.TotalCostUSD > 0))
	if costUsable {
		t.sawCost = true
		t.costUSD += *ev.TotalCostUSD
	} else {
		t.costIncomplete = true
	}
	t.event(Event{Kind: "usage", Usage: t.usage, CostUSD: t.costUSD, UsageKnown: t.sawUsage && !t.usageIncomplete, CostKnown: t.sawCost && !t.costIncomplete})

	// The trailer the UI renders as the run's token count. A resumed run
	// appends a second one, mirroring codex's per-invocation trailers. The
	// cost line rides along so a live log shows spend without waiting for
	// the review to land in history; codex reports none, so it stays off
	// there rather than printing a misleading zero.
	if t.sawUsage && !t.usageIncomplete {
		_, _ = fmt.Fprintf(t.out, "tokens used\n%s\n", withThousands(t.usage.Total()))
	} else if t.sawUsage {
		_, _ = fmt.Fprintf(t.out, "usage incomplete; recorded tokens: %s\n", withThousands(t.usage.Total()))
	} else {
		_, _ = fmt.Fprintln(t.out, "usage unavailable")
	}
	if t.costUSD > 0 {
		if t.sawCost && !t.costIncomplete {
			_, _ = fmt.Fprintf(t.out, "~ $%.4f at API rates\n", t.costUSD)
		} else {
			_, _ = fmt.Fprintf(t.out, "~ $%.4f recorded at API rates; total unavailable\n", t.costUSD)
		}
	}
}

// resultFailure describes a result event that carries no usable report: the
// CLI's is_error flag, its subtype, and whatever it put in `result`. "" when
// the run reported an outcome normally.
func resultFailure(ev streamEvent) string {
	hasReport := len(ev.StructuredOutput) > 0 && !bytes.Equal(bytes.TrimSpace(ev.StructuredOutput), []byte("null"))
	if hasReport && !ev.IsError {
		return ""
	}
	parts := []string{}
	if ev.Subtype != "" && ev.Subtype != "success" {
		parts = append(parts, ev.Subtype)
	}
	if text := decodeResultText(ev.Result); text != "" {
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		if ev.IsError {
			return "the run reported an error with no detail"
		}
		if !hasReport {
			return "the run ended without a report"
		}
		return ""
	}
	return strings.Join(parts, ": ")
}

// decodeResultText reads the result payload, which is a plain string for most
// outcomes and an object for some; an unrecognised shape falls back to its raw
// form rather than being dropped.
func decodeResultText(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(string(raw))
}

// decodeContent reads a message's content, which is an array of blocks for
// assistant messages and either an array or a bare string for user messages.
func decodeContent(raw json.RawMessage) []contentBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return []contentBlock{{Type: "text", Text: text}}
	}
	return nil
}

// decodeToolResult flattens a tool result's content, which is a string for
// most tools and a content-block array for tools that return structured parts.
func decodeToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimRight(text, "\n")
	}
	var parts []contentBlock
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, p := range parts {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		return strings.TrimRight(strings.Join(out, "\n"), "\n")
	}
	return strings.TrimRight(string(raw), "\n")
}

// renderToolUse turns a tool call into the one-line command the exec marker
// expects. Bash is the overwhelming majority of a review's tool traffic and
// its command reads naturally; everything else renders as name + arguments.
func renderToolUse(b contentBlock) string {
	if b.Name == "Bash" {
		var input struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(b.Input, &input); err == nil && input.Command != "" {
			return input.Command
		}
	}
	if len(b.Input) == 0 {
		return b.Name
	}
	return b.Name + " " + string(b.Input)
}
