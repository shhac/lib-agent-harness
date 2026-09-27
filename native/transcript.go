package native

// The transcript substrate both engines render into. AGENTS.md states that
// engines differ only in how they spawn a CLI and that both render their own
// stream into the SAME marker format so agent.log stays one format and the
// dashboard needs one parser. That claim was true of the OUTPUT and false of
// the code: each transcoder carried its own copy of the line splitting, the
// marker writer, and the prompt banner, so the one shared contract had two
// implementations to keep in sync and only one of them was directly tested.
//
// What genuinely differs per engine is reading an event and deciding what to
// render, which stays in codexstream.go and claudestream.go as consume().

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// markerSink owns everything about turning a byte stream into marker blocks:
// reassembling lines across chunk boundaries, writing a block, and holding the
// prompt until the agent's first activity. Embedded by both transcoders.
type markerSink struct {
	onEvent func(Event)
	out     io.Writer
	partial []byte // carry for a chunk that split mid-line

	// pendingPrompt is registered by the driver and rendered on first
	// activity. Neither CLI echoes the prompt back as a stream event the way
	// codex's prose output used to, and rendering it lazily rather than on
	// arrival is what keeps the session banner above it: the event carrying
	// the session id only arrives after the driver has handed the prompt over.
	pendingPrompt string
}

// writeLines feeds whole lines to consume, carrying any partial tail to the
// next call. The io.Writer half of both transcoders is this and nothing else.
func (m *markerSink) writeLines(p []byte, consume func([]byte)) (int, error) {
	m.partial = append(m.partial, p...)
	for {
		i := bytes.IndexByte(m.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := m.partial[:i]
		m.partial = m.partial[i+1:]
		consume(line)
	}
}

// flushPartial renders a trailing line the stream ended without a newline on,
// then the prompt if no event ever arrived to flush it: a run that died before
// its first message still shows what it was asked.
func (m *markerSink) flushPartial(consume func([]byte)) {
	if len(bytes.TrimSpace(m.partial)) > 0 {
		consume(m.partial)
	}
	m.partial = nil
	m.flushPrompt()
}

func (m *markerSink) userPrompt(text string) { m.pendingPrompt = text }

// flushPrompt renders the registered prompt, once, immediately before the
// first thing the agent does with it.
func (m *markerSink) flushPrompt() {
	if m.pendingPrompt == "" {
		return
	}
	text := m.pendingPrompt
	m.pendingPrompt = ""
	m.emit("user", text)
}

// emit writes one marker block. Write errors are deliberately dropped: the
// sink is a best-effort log tee that already degrades to buffer-only when the
// workspace cannot hold a file (see newAgentSink), and failing a review
// because its transcript could not be written would trade a cosmetic loss for
// a real one.
func (m *markerSink) emit(marker, body string) {
	body = strings.TrimRight(body, "\n")
	if marker == "" {
		_, _ = fmt.Fprintf(m.out, "%s\n", body)
		return
	}
	_, _ = fmt.Fprintf(m.out, "%s\n%s\n", marker, body)
}

// withThousands formats a token count the way codex prints its trailer, so
// the UI renders both engines' counts identically. Shared, not codex's: the
// claude transcoder prints the same trailer.
func withThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// extractUsage pulls the verbatim usage object out of an event line, so the
// history row can keep what the engine actually said alongside the fields we
// model. Fields we never map (service tiers, per-turn breakdowns, whatever
// either CLI adds next) survive in the store without needing a schema change.
// Shared for the same reason as withThousands: both transcoders call it.
func extractUsage(line []byte) json.RawMessage {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil
	}
	if len(bytes.TrimSpace(envelope.Usage)) == 0 {
		return nil
	}
	return envelope.Usage
}

func (m *markerSink) event(e Event) {
	if m.onEvent != nil {
		m.onEvent(e)
	}
}

// toolEnded writes the marker line that closes a tool call. timed is false
// when the call's start was never seen, so no duration can be stated.
func (m *markerSink) toolEnded(failed bool, elapsed time.Duration, timed bool, output string) {
	status := "succeeded"
	if failed {
		status = "failed"
	}
	duration := ""
	if timed {
		duration = " in " + elapsed.Truncate(10*time.Millisecond).String()
	}
	_, _ = fmt.Fprintf(m.out, " %s%s:\n%s\n", status, duration, output)
}

// spendTrailer writes the per-invocation lines the UI renders as a run's token
// count; a resumed run appends another. The cost line rides along so a live
// log shows spend without waiting for the run to land in history; an engine
// that reports no cost passes zero, so no misleading zero is printed.
// Total counts every prompt token, cached reads included.
func (m *markerSink) spendTrailer(usage harness.Usage, sawUsage bool, cost harness.Cost) {
	switch {
	case usage.Known:
		_, _ = fmt.Fprintf(m.out, "tokens used\n%s\n", withThousands(usage.Total()))
	case sawUsage:
		_, _ = fmt.Fprintf(m.out, "usage incomplete; recorded tokens: %s\n", withThousands(usage.Total()))
	default:
		_, _ = fmt.Fprintln(m.out, "usage unavailable")
	}
	switch {
	case cost.USD <= 0:
	case cost.Known:
		_, _ = fmt.Fprintf(m.out, "~ $%.4f at API rates\n", cost.USD)
	default:
		_, _ = fmt.Fprintf(m.out, "~ $%.4f recorded at API rates; total unavailable\n", cost.USD)
	}
}

// jsonAbsent reports a missing value or an explicit JSON null.
func jsonAbsent(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
