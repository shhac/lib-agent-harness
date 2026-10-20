package native

// Antigravity's `--output-format=stream-json` is NDJSON, one `event`-tagged
// object per line (verified against agy 1.4.2: `init`, `step_update`, and
// terminal `result`). A result's usage covers the whole conversation, so a
// resumed run would count earlier runs again: a run's usage is the sum of its
// own agent_response steps instead.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
	"github.com/shhac/lib-agent-harness/internal/tokens"
)

type antigravityWireEvent struct {
	Event          string                 `json:"event"`
	ConversationID string                 `json:"conversation_id"`
	StepUpdate     *antigravityStepUpdate `json:"step_update"`
	Result         *antigravityResult     `json:"result"`
	Error          *antigravityError      `json:"error"`
}

type antigravityError = nativecli.AntigravityError

type antigravityStepUpdate struct {
	StepIndex     int                  `json:"step_index"`
	StepType      string               `json:"step_type"`
	State         string               `json:"state"`
	TextDelta     string               `json:"text_delta"`
	ThinkingDelta string               `json:"thinking_delta"`
	ToolName      string               `json:"tool_name"`
	ToolInfo      *antigravityToolInfo `json:"tool_info"`
	Usage         json.RawMessage      `json:"usage"`
}

type antigravityToolInfo struct {
	Name       string            `json:"name"`
	Parameters json.RawMessage   `json:"parameters"`
	Output     string            `json:"output"`
	Error      *antigravityError `json:"error"`
}

type antigravityResult struct {
	Status           string            `json:"status"`
	Response         string            `json:"response"`
	StructuredOutput json.RawMessage   `json:"structured_output"`
	Error            *antigravityError `json:"error"`
}

// antigravityUsage is agy's JSONUsage on an agent_response step: one model
// response. input_tokens is uncached input, with cache_read_tokens beside it
// (observed on agy 1.4.2: a repeated prefix moved from one to the other).
// thinking_tokens is reasoning output reported beside output_tokens.
type antigravityUsage struct {
	InputTokens     *int64 `json:"input_tokens"`
	OutputTokens    *int64 `json:"output_tokens"`
	ThinkingTokens  *int64 `json:"thinking_tokens"`
	CacheReadTokens *int64 `json:"cache_read_tokens"`
}

// usage is not known unless agy states both input and output counts. Its
// output excludes the thinking reported beside it, and agy never writes to the
// cache, so a stated read is the whole split.
func (u antigravityUsage) usage() (harness.Usage, bool) {
	output := u.OutputTokens
	if u.ThinkingTokens != nil {
		output = tokens.Sum(u.OutputTokens, u.ThinkingTokens)
	}
	usage := tokens.CacheExclusive(tokens.Report{Input: u.InputTokens, Output: output, CacheRead: u.CacheReadTokens, CacheWrite: tokens.NoCacheWrite(u.CacheReadTokens), Reasoning: u.ThinkingTokens})
	return usage, usage.Known
}

type antigravityTranscoder struct {
	markerSink

	completed       bool
	sawUsage        bool
	usageIncomplete bool
	sessionID       string
	usage           harness.Usage // summed across every invocation
	rawUsage        []json.RawMessage
	report          json.RawMessage
	failure         string
	authFailed      bool
	text            strings.Builder
	lastResponse    string
	startedTools    map[string]bool
	completedTools  map[string]bool
	countedSteps    map[int]bool
	running         map[string]time.Time
}

func newAntigravityTranscoder(out io.Writer) *antigravityTranscoder {
	return &antigravityTranscoder{
		markerSink:     newMarkerSink(out),
		startedTools:   map[string]bool{},
		completedTools: map[string]bool{},
		countedSteps:   map[int]bool{},
		running:        map[string]time.Time{},
	}
}

func (t *antigravityTranscoder) Write(p []byte) (int, error) { return t.writeLines(p, t.consume) }

func (t *antigravityTranscoder) beginTurn(prompt string) {
	t.completed = false
	t.report = nil
	t.failure = ""
	t.authFailed = false
	t.text.Reset()
	t.lastResponse = ""
	clear(t.startedTools)
	clear(t.completedTools)
	clear(t.countedSteps)
	clear(t.running)
	t.userPrompt(prompt)
}

func (t *antigravityTranscoder) reachedTerminal() bool { return t.completed }

func (t *antigravityTranscoder) failureCause() (harness.Cause, *time.Time) {
	if t.authFailed {
		return harness.CauseAuthentication, nil
	}
	return "", nil
}

func (t *antigravityTranscoder) snapshot() Result {
	usage := t.usage
	usage.Known = t.completed && t.sawUsage && !t.usageIncomplete
	return Result{
		SessionID: t.sessionID,
		Report:    append(json.RawMessage(nil), t.report...),
		Usage:     usage,
		RawUsage:  joinRawUsage(t.rawUsage),
		Failure:   t.failure,
	}
}

func (t *antigravityTranscoder) Close() {
	t.flushPartial(t.consume)
	if !t.completed {
		t.usageIncomplete = true
	}
}

func (t *antigravityTranscoder) consume(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var ev antigravityWireEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Event == "" {
		t.emit("", string(line))
		return
	}
	if ev.ConversationID != "" && ev.ConversationID != t.sessionID {
		t.sessionID = ev.ConversationID
		t.event(Event{Kind: "session", SessionID: ev.ConversationID})
		t.emit("", "session id: "+ev.ConversationID)
	}

	switch ev.Event {
	case "step_update":
		if ev.StepUpdate != nil {
			t.consumeStepUpdate(*ev.StepUpdate)
		}
	case "error":
		msg := ""
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		t.renderFailure(msg)
	case "result":
		if ev.Result != nil {
			t.consumeResult(*ev.Result)
		}
	}
}

func (t *antigravityTranscoder) consumeStepUpdate(u antigravityStepUpdate) {
	switch u.StepType {
	case "agent_response":
		if u.ThinkingDelta != "" {
			t.flushPrompt()
			t.event(Event{Kind: "reasoning", Text: u.ThinkingDelta})
			t.emit("thinking", u.ThinkingDelta)
		}
		if u.TextDelta != "" {
			t.flushPrompt()
			t.text.WriteString(u.TextDelta)
			t.event(Event{Kind: "message", Text: u.TextDelta})
			t.emit("antigravity", u.TextDelta)
		}
		if u.State == "DONE" && t.text.Len() > 0 {
			t.lastResponse = t.text.String()
			t.text.Reset()
		}
		if u.State == "DONE" {
			t.countStepUsage(u)
		}
	case "tool":
		t.consumeToolUpdate(u)
	}
}

// countStepUsage counts each response once. A response without both counts
// leaves the run's usage unknown rather than undercounted.
func (t *antigravityTranscoder) countStepUsage(u antigravityStepUpdate) {
	if t.countedSteps[u.StepIndex] {
		return
	}
	t.countedSteps[u.StepIndex] = true
	var parsed antigravityUsage
	if rawjson.Absent(u.Usage) || json.Unmarshal(u.Usage, &parsed) != nil {
		t.usageIncomplete = true
		return
	}
	usage, ok := parsed.usage()
	if !ok {
		t.usageIncomplete = true
		return
	}
	t.rawUsage = append(t.rawUsage, append(json.RawMessage(nil), u.Usage...))
	t.usage, ok = addUsage(t.usage, usage)
	t.usageIncomplete = t.usageIncomplete || !ok
	t.sawUsage = true
}

func (t *antigravityTranscoder) consumeToolUpdate(u antigravityStepUpdate) {
	itemID := fmt.Sprintf("step-%d", u.StepIndex)
	name := u.ToolName
	var input json.RawMessage
	var output string
	if u.ToolInfo != nil {
		if name == "" {
			name = u.ToolInfo.Name
		}
		if !rawjson.Absent(u.ToolInfo.Parameters) {
			input = append(json.RawMessage(nil), u.ToolInfo.Parameters...)
		}
		output = u.ToolInfo.Output
		if output == "" && u.ToolInfo.Error != nil {
			output = u.ToolInfo.Error.Message
		}
	}
	if !t.startedTools[itemID] {
		t.startedTools[itemID] = true
		t.flushPrompt()
		t.event(Event{Kind: "tool_start", ItemID: itemID, ToolName: name, Input: input})
		t.running[itemID] = t.now()
		desc := name
		if len(input) > 0 {
			desc = name + "\n" + string(input)
		}
		t.emit("exec", desc)
	}
	switch u.State {
	case "DONE", "ERROR", "CANCELED", "INTERRUPTED":
		if t.completedTools[itemID] {
			return
		}
		t.completedTools[itemID] = true
		failed := u.State != "DONE"
		t.event(Event{Kind: "tool_end", ItemID: itemID, Output: output, Failed: failed})
		var elapsed time.Duration
		started, timed := t.running[itemID]
		if timed {
			elapsed = t.now().Sub(started)
			delete(t.running, itemID)
		}
		t.toolEnded(failed, elapsed, timed, output)
	}
}

func (t *antigravityTranscoder) consumeResult(res antigravityResult) {
	t.flushPrompt()
	if res.Response != "" && t.finalText() == "" {
		t.text.WriteString(res.Response)
		t.event(Event{Kind: "message", Text: res.Response})
		t.emit("antigravity", res.Response)
	}
	t.completed = true

	t.authFailed = res.Error != nil && nativecli.AntigravityAuthFailure(res.Error.Message)
	report := t.resultReport(res)
	failure := antigravityResultFailure(res, t.structured, len(report) > 0)
	if failure == "" && t.failure == "" {
		t.report = report
	} else if failure != "" {
		t.renderFailure(failure)
	}

	usage := t.usage
	usage.Known = t.sawUsage && !t.usageIncomplete
	var cost harness.Cost
	t.event(Event{Kind: "usage", Usage: usage, Cost: cost})
	t.spendTrailer(usage, t.sawUsage, cost)
}

func (t *antigravityTranscoder) resultReport(res antigravityResult) json.RawMessage {
	if t.structured {
		if rawjson.Absent(res.StructuredOutput) {
			return nil
		}
		return append(json.RawMessage(nil), res.StructuredOutput...)
	}
	text := res.Response
	if text == "" {
		text = t.finalText()
	}
	if text == "" {
		return nil
	}
	report, _ := json.Marshal(text)
	return report
}

func (t *antigravityTranscoder) finalText() string {
	if t.text.Len() > 0 {
		return t.text.String()
	}
	return t.lastResponse
}

func (t *antigravityTranscoder) renderFailure(failure string) {
	if failure == "" {
		failure = "harness turn failed"
	}
	t.flushPrompt()
	t.failure = failure
	t.event(Event{Kind: "error", Text: failure})
	t.emit("error", failure)
}

func antigravityResultFailure(res antigravityResult, structured, hasReport bool) string {
	if res.Status != "SUCCESS" || res.Error != nil {
		if res.Error != nil && res.Error.Message != "" {
			return res.Error.Message
		}
		if res.Status != "" {
			return "the run stopped: " + res.Status
		}
		return "the run reported an error with no detail"
	}
	if structured && !hasReport {
		return "the run ended without structured output"
	}
	return ""
}
