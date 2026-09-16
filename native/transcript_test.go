package native

import (
	"bytes"
	"strings"
	"testing"
)

// The substrate both engines share now has one implementation and therefore
// one place to test. Line reassembly in particular was duplicated and only
// exercised through the claude transcoder, so codex's identical copy was
// verified by inference rather than by a test.
func TestMarkerSinkReassemblesSplitLines(t *testing.T) {
	var buf bytes.Buffer
	m := &markerSink{out: &buf}

	var got []string
	consume := func(line []byte) { got = append(got, string(line)) }

	// A line split across three writes, and two lines in one write.
	for _, chunk := range []string{"hel", "lo wor", "ld\nsecond\nthird\n"} {
		if n, err := m.writeLines([]byte(chunk), consume); err != nil || n != len(chunk) {
			t.Fatalf("writeLines(%q) = %d, %v; want %d, nil", chunk, n, err, len(chunk))
		}
	}
	want := []string{"hello world", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A stream that ends without a trailing newline must still render its last
// line, and a run that died before any event arrived must still show what it
// was asked.
func TestMarkerSinkFlushPartialRendersTailAndPrompt(t *testing.T) {
	var buf bytes.Buffer
	m := &markerSink{out: &buf}
	m.userPrompt("REVIEW THIS")

	var got []string
	consume := func(line []byte) { got = append(got, string(line)) }
	_, _ = m.writeLines([]byte("no trailing newline"), consume)
	if len(got) != 0 {
		t.Fatalf("an unterminated line must wait for the flush, got %q", got)
	}

	m.flushPartial(consume)
	if len(got) != 1 || got[0] != "no trailing newline" {
		t.Errorf("flush must render the tail, got %q", got)
	}
	if !strings.Contains(buf.String(), "user\nREVIEW THIS\n") {
		t.Errorf("flush must render the unflushed prompt, got %q", buf.String())
	}

	// The prompt renders once, never twice.
	buf.Reset()
	m.flushPrompt()
	if buf.Len() != 0 {
		t.Errorf("prompt rendered a second time: %q", buf.String())
	}
}

func TestMarkerSinkEmit(t *testing.T) {
	var buf bytes.Buffer
	m := &markerSink{out: &buf}
	m.emit("agent", "body\n\n")
	m.emit("", "bare")
	if got, want := buf.String(), "agent\nbody\nbare\n"; got != want {
		t.Errorf("emit output = %q, want %q", got, want)
	}
}
