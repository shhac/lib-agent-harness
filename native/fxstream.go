package native

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// fx's noninteractive run writes one JSON document to standard output when it
// is asked for machine-readable output, and its progress and diagnostics to
// standard error. The library reads both through one stream, so the document
// is found by its shape and everything else is left alone.

// fxDocument is the document `fx ask --json` writes.
type fxDocument struct {
	Output      string `json:"output"`
	FinalOutput string `json:"final_output"`
	Error       string `json:"error"`
	ExitCode    *int   `json:"exit_code"`
	Model       string `json:"model"`
	SessionID   string `json:"session_id"`
	Steps       *int   `json:"steps"`
	Usage       *struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
	} `json:"usage"`
	ToolCalls []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"tool_calls"`
}

// fxTranscoder is one fx run's half of Stream. fx answers with a document
// rather than a stream of events, so the whole run is one record: the final
// response, the tools it used, and the usage it reported.
type fxTranscoder struct {
	markerSink

	structured bool
	completed  bool
	sawUsage   bool
	sessionID  string
	usage      harness.Usage
	report     json.RawMessage
	failure    string
	now        func() time.Time
}

func newFxTranscoder(out io.Writer) *fxTranscoder {
	return &fxTranscoder{markerSink: markerSink{out: out}, now: time.Now}
}

func (t *fxTranscoder) Write(p []byte) (int, error) { return t.writeLines(p, t.consume) }

// beginTurn clears the previous report and failure. A run that reported no
// usage leaves the session's accounting unknown, never zero.
func (t *fxTranscoder) beginTurn(prompt string) {
	t.completed = false
	t.sawUsage = false
	t.report = nil
	t.failure = ""
	t.userPrompt(prompt)
}

func (t *fxTranscoder) reachedTerminal() bool { return t.completed }

// failureCause names why a failed run failed. fx states the reason in its own
// prose, which is kept in the result and never parsed for a cause.
func (t *fxTranscoder) failureCause() (harness.Cause, *time.Time) { return harness.CauseUnknown, nil }

func (t *fxTranscoder) snapshot() Result {
	usage := t.usage
	usage.Known = t.completed && t.sawUsage
	return Result{
		SessionID: t.sessionID,
		Report:    append(json.RawMessage(nil), t.report...),
		Usage:     usage,
		Failure:   t.failure,
	}
}

// Close flushes a trailing line. A run that never wrote its document has no
// answer, which the caller is told rather than read as an empty one.
func (t *fxTranscoder) Close() { t.flushPartial(t.consume) }

// consume reads one line. Only the run's document is read; fx's notices, and
// any other output, are not part of the run's record.
func (t *fxTranscoder) consume(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(trimmed, &probe) != nil {
		return
	}
	if _, ok := probe["final_output"]; !ok {
		return
	}
	if _, ok := probe["output"]; !ok {
		return
	}
	var doc fxDocument
	if json.Unmarshal(trimmed, &doc) != nil {
		return
	}
	t.record(doc)
}

// record folds the run's document into the transcoder's state.
func (t *fxTranscoder) record(doc fxDocument) {
	t.completed = true
	if doc.SessionID != "" {
		t.sessionID = doc.SessionID
		t.event(Event{Kind: "session", SessionID: doc.SessionID})
		t.emit("", "session id: "+doc.SessionID)
	}
	if u := doc.Usage; u != nil && u.Input != nil && u.Output != nil {
		t.usage = harness.Usage{Known: true, Input: *u.Input, Output: *u.Output}
		t.sawUsage = true
	}
	t.flushPrompt()
	for _, call := range doc.ToolCalls {
		t.event(Event{Kind: "tool_start", ToolName: call.Name})
		t.emit("tool", call.Name)
		t.event(Event{Kind: "tool_end", ToolName: call.Name, Failed: fxToolFailed(call.Status)})
	}
	text := doc.FinalOutput
	if text == "" {
		text = doc.Output
	}
	if doc.ExitCode != nil && *doc.ExitCode != 0 {
		// The failure text is provider prose. It is kept in the result, where
		// the caller can show it deliberately, and never becomes an error value.
		t.failure = fxFailureText(doc)
		t.event(Event{Kind: "error", Text: t.failure})
		t.emit("error", t.failure)
		return
	}
	if text != "" {
		if t.structured {
			// fx states no output schema, so there is no schema-shaped report
			// to give. A structured run fails rather than answering with prose.
			t.failure = "fx states no output schema for a structured run"
			t.event(Event{Kind: "error", Text: t.failure})
			t.emit("error", t.failure)
			return
		}
		t.event(Event{Kind: "message", Text: text})
		t.emit("fx", text)
		report, _ := json.Marshal(text)
		t.report = report
	}
	usage := t.usage
	usage.Known = t.sawUsage
	t.event(Event{Kind: "usage", Usage: usage})
	t.spendTrailer(usage, t.sawUsage, harness.Cost{})
}

// fxToolFailed reads a tool call's stated status. A status this library does
// not know is not a failure; only a stated one is.
func fxToolFailed(status string) bool {
	switch status {
	case "error", "failed", "cancelled", "denied", "rejected":
		return true
	}
	return false
}

// fxFailureText is what fx said went wrong, or a plain statement that it
// failed without saying why. A failed run's output is the assistant's text so
// far, such as what it was about to do, so it is used only when fx named no
// error.
func fxFailureText(doc fxDocument) string {
	if doc.Error != "" {
		return doc.Error
	}
	if text := doc.Output; text != "" {
		return text
	}
	return "the run failed"
}
