package native

// `codex exec --json` emits newline-delimited JSON instead of the readable
// transcript `codex exec` prints natively. This file converts one into the
// other as the run streams, into the same bare-marker format claude's
// transcoder produces, so agent.log stays a single cross-engine contract and
// the dashboard's one parser serves both drivers (see
// internal/dashboard/ui/src/lib/agentlog.ts).
//
// Reading the JSON rather than the prose is what makes the token split
// available: the prose trailer is a single cache-excluded number, while
// turn.completed carries input, cached, output and reasoning separately.
//
// The cost of the switch is that agent.log is now rendered by us rather than
// printed by codex, so an item type this file does not know renders as
// nothing. The known set is pinned by a golden transcript test; the trade is
// the same one claude's transcoder already makes.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// codexEvent is the subset of codex's --json protocol this driver reads.
// Unknown types and fields are ignored by design: the protocol grows, and an
// unrecognised event must degrade to "nothing rendered", never to a failed
// review.
type codexEvent struct {
	Error    json.RawMessage `json:"error"`
	Message  string          `json:"message"`
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     *codexItem      `json:"item"`
	Usage    *codexUsage     `json:"usage"`
}

// codexItem is one unit of run activity. The fields are a union across item
// types: agent_message carries Text, error carries Message, and
// command_execution carries the command with its output and exit code.
type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"item_type"`
	AltType          string `json:"type"`
	Text             string `json:"text"`
	Message          string `json:"message"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
}

// kind reads the item's type from whichever key this codex build used.
func (i codexItem) kind() string {
	if i.Type != "" {
		return i.Type
	}
	return i.AltType
}

// codexUsage is one turn.completed's usage report.
//
// InputTokens INCLUDES CachedInputTokens (measured: a run reporting
// input 18,389 / cached 10,496 printed a prose trailer of 7,928, which is
// input - cached + output, not input + output). So the fresh input is the
// difference, and treating input_tokens as fresh would double-count every
// cached read.
//
// ReasoningOutputTokens is a SUBSET of OutputTokens, recorded for analysis
// and never added into a total.
type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// tokenUsage maps codex's report onto the engine-agnostic split. codex has no
// explicit cache write (its caching is implicit), so CacheWrite stays 0.
func (u codexUsage) tokenUsage() TokenUsage {
	return TokenUsage{
		Input:     u.InputTokens - u.CachedInputTokens,
		Output:    u.OutputTokens,
		CacheRead: u.CachedInputTokens,
		Reasoning: u.ReasoningOutputTokens,
	}
}

// codexTranscoder consumes `codex exec --json` stdout and writes the marker
// transcript into out, accumulating the state the resume policy reads back.
// It implements io.Writer so the subprocess seam matches claude's: production
// hands it the process stdout, tests write a canned stream into it directly.
type codexTranscoder struct {
	markerSink // line reassembly, marker writing, the prompt banner

	report     json.RawMessage
	failure    string
	completed  bool
	structured bool
	threadID   string
	usage      TokenUsage
	rawUsage   []json.RawMessage // every turn.completed usage, verbatim
	running    map[string]time.Time
	sawUsage   bool

	now func() time.Time // injectable clock so the rendered durations are testable
}

func newCodexTranscoder(out io.Writer) *codexTranscoder {
	return &codexTranscoder{markerSink: markerSink{out: out}, running: map[string]time.Time{}, now: time.Now}
}

func (t *codexTranscoder) Write(p []byte) (int, error) { return t.writeLines(p, t.consume) }

// Close renders any trailing line the stream ended without a newline on, plus
// a prompt no event ever arrived to flush (a run that died before its first
// message still shows what it was asked), then the token trailer.
func (t *codexTranscoder) Close() {
	t.flushPartial(t.consume)
	if t.sawUsage {
		_, _ = fmt.Fprintf(t.out, "tokens used\n%s\n", withThousands(t.usage.Fresh()))
	}
}

// consume renders one stream line. A line that isn't a JSON event is passed
// through verbatim: codex prints the occasional plain warning, and dropping
// them would lose diagnostics the log is meant to preserve.
func (t *codexTranscoder) consume(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.emit("", string(line))
		return
	}
	if ev.ThreadID != "" {
		t.threadID = ev.ThreadID
		t.event(Event{Kind: "session", SessionID: ev.ThreadID})
		t.emit("", "session id: "+ev.ThreadID)
		return
	}
	if ev.Type == "turn.failed" || ev.Type == "error" {
		t.failure = ev.Message
		if t.failure == "" {
			t.failure = decodeResultText(ev.Error)
		}
		if t.failure == "" {
			t.failure = "harness turn failed"
		}
		t.event(Event{Kind: "error", Text: t.failure})
		t.emit("error", t.failure)
		return
	}
	if ev.Type == "turn.completed" {
		t.completed = true
		t.recordUsage(ev, line)
		return
	}
	if ev.Item == nil {
		return
	}
	// Everything past the banner counts as the run getting underway, so the
	// prompt goes in just above it.
	t.flushPrompt()
	t.renderItem(ev.Type, *ev.Item)
}

// recordUsage REPLACES rather than accumulates: codex reports the session
// total on every turn, so a resumed run's figure already contains the first
// invocation's (measured across three turns: output 209 -> 230 -> 251, cached
// rising by exactly 18,176 each time, reasoning pinned at 32). Summing them,
// which the prose trailer's shape invited, double-counted every resumed run.
// claude is the opposite and its transcoder sums; the difference is engine
// knowledge and so is stated once, here, per driver.
func (t *codexTranscoder) recordUsage(ev codexEvent, line []byte) {
	if ev.Usage == nil {
		return
	}
	t.usage = ev.Usage.tokenUsage()
	t.sawUsage = true
	t.event(Event{Kind: "usage", Usage: t.usage})
	if raw := extractUsage(line); raw != nil {
		t.rawUsage = append(t.rawUsage, raw)
	}
}

func (t *codexTranscoder) renderItem(eventType string, item codexItem) {
	switch item.kind() {
	case "agent_message":
		if eventType == "item.completed" {
			if t.structured {
				t.report = json.RawMessage(item.Text)
			} else {
				t.report, _ = json.Marshal(item.Text)
			}
		}
		t.event(Event{Kind: "message", ItemID: item.ID, Text: item.Text})
		t.emit("codex", item.Text)
	case "reasoning":
		t.event(Event{Kind: "reasoning", ItemID: item.ID, Text: item.Text})
		t.emit("thinking", item.Text)
	case "error":
		t.event(Event{Kind: "error", ItemID: item.ID, Text: item.Message})
		t.emit("error", item.Message)
	case "command_execution":
		t.renderCommand(eventType, item)
	}
}

// renderCommand pairs a command with its result by item id rather than by
// arrival order: codex gives every item an id, so interleaved parallel calls
// attribute exactly, where claude's protocol leaves it a FIFO guess.
func (t *codexTranscoder) renderCommand(eventType string, item codexItem) {
	if eventType == "item.started" {
		t.event(Event{Kind: "tool_start", ItemID: item.ID, ToolName: "command_execution", Text: item.Command})
		t.running[item.ID] = t.now()
		t.emit("exec", item.Command)
		return
	}
	if eventType == "item.updated" {
		t.event(Event{Kind: "tool_output", ItemID: item.ID, ToolName: "command_execution", Output: item.AggregatedOutput})
		return
	}
	if eventType != "item.completed" {
		return
	}
	// A completion with no matching start (a command fast enough that codex
	// emitted only the one event) still needs its command line rendered, or
	// the result would attach to whatever ran before it.
	started, ok := t.running[item.ID]
	if !ok {
		t.emit("exec", item.Command)
	}
	delete(t.running, item.ID)
	t.event(Event{Kind: "tool_end", ItemID: item.ID, ToolName: "command_execution", Output: item.AggregatedOutput, Failed: item.ExitCode == nil || *item.ExitCode != 0})

	status := "succeeded"
	if item.ExitCode == nil || *item.ExitCode != 0 {
		status = "failed"
	}
	elapsed := ""
	if ok {
		elapsed = " in " + t.now().Sub(started).Truncate(10*time.Millisecond).String()
	}
	_, _ = fmt.Fprintf(t.out, " %s%s:\n%s\n", status, elapsed, strings.TrimRight(item.AggregatedOutput, "\n"))
}
