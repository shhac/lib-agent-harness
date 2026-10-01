package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/completion"
)

// A bound holds with its marker: a truncated text never exceeds the limit,
// never splits a character, and says it was cut whenever a marker fits.
func TestBoundKeepsItsMarkerInsideTheLimit(t *testing.T) {
	text := strings.Repeat("abcé", 400)
	for limit := 0; limit <= 2*len(text); limit++ {
		got := bound(text, limit)
		switch {
		case limit >= len(text):
			if got != text {
				t.Fatalf("limit %d cut a text that fits", limit)
			}
			continue
		case len(got) > limit || !utf8.ValidString(got):
			t.Fatalf("limit %d: %d bytes, valid %v", limit, len(got), utf8.ValidString(got))
		case limit >= 64 && !strings.Contains(got, " further bytes omitted]"):
			t.Fatalf("limit %d lost the full marker: %q", limit, got[max(0, len(got)-60):])
		case limit >= len("[truncated]") && !strings.Contains(got, "[truncated"):
			t.Fatalf("limit %d cut silently: %q", limit, got)
		}
		if i := strings.Index(got, "\n[truncated: "); i >= 0 {
			count, _, _ := strings.Cut(strings.TrimPrefix(got[i:], "\n[truncated: "), " ")
			if omitted, err := strconv.Atoi(count); err != nil || i+omitted != len(text) {
				t.Fatalf("limit %d: marker %q does not count the %d bytes omitted", limit, got[i:], len(text)-i)
			}
		}
	}
}

// The host's MaxResultBytes bounds the result the model sees, marker
// included, however small it is.
func TestHostResultFitsMaxResultBytes(t *testing.T) {
	for _, limit := range []int{5, 40, 100, 4096} {
		big := strings.Repeat("x", 10*limit)
		host := newDirectToolHost(ToolHost{
			Server:         "work",
			Tools:          []ToolDefinition{{Name: "dump", Schema: map[string]any{"type": "object"}}},
			Handler:        ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{Content: big}, nil }),
			MaxResultBytes: limit,
		})
		ready, refusal := host.prepareCall("r1", "dump", json.RawMessage(`{}`))
		if refusal != nil {
			t.Fatalf("refused: %+v", refusal)
		}
		out := host.execute(ready, "turn", nil)
		if len(out.text) > limit || (limit >= 11 && !strings.Contains(out.text, "[truncated")) {
			t.Fatalf("limit %d: %d bytes %q", limit, len(out.text), out.text[max(0, len(out.text)-60):])
		}
		host.close()
	}
}

// Every other outcome is bounded the same way: a handler's error, a
// cancellation, and each refusal, whether at admission or after it. What is
// recorded is what is bounded.
func TestHostEveryOutcomeFitsMaxResultBytes(t *testing.T) {
	for _, limit := range []int{5, 40} {
		failing := errors.New(strings.Repeat("handler failure ", 400))
		handler := ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
			if c.Name == "cancelled" {
				return ToolResult{}, context.Canceled
			}
			return ToolResult{}, failing
		})
		schema := map[string]any{"type": "object"}
		host := newDirectToolHost(ToolHost{Server: "work", Handler: handler, MaxResultBytes: limit, Tools: []ToolDefinition{{Name: "fails", Schema: schema}, {Name: "cancelled", Schema: schema}}})
		check := func(what string, out toolOutcome) {
			t.Helper()
			if len(out.text) > limit || out.text == "" {
				t.Errorf("limit %d, %s: %d bytes %q", limit, what, len(out.text), out.text)
			}
		}
		for _, name := range []string{"fails", "cancelled"} {
			ready, refusal := host.prepareCall("r-"+name, name, json.RawMessage(`{}`))
			if refusal != nil {
				t.Fatalf("refused: %+v", refusal)
			}
			var recorded toolOutcome
			out := host.execute(ready, "turn", func(o toolOutcome) { recorded = o })
			check(name, out)
			check(name+" as recorded", recorded)
		}
		_, refusal := host.prepareCall("r-unknown", strings.Repeat("x", 60), json.RawMessage(`{}`))
		check("unknown tool", *refusal)
		_, refusal = host.prepareCall("r-malformed", "fails", json.RawMessage(`[1]`))
		check("malformed arguments", *refusal)
		host.pause()
		_, refusal = host.prepareCall("r-paused", "fails", json.RawMessage(`{}`))
		check("paused channel", *refusal)
		host.close()
	}
}

// The loop's own answers, for a call that never ran or whose outcome is
// unknown, are bounded by the same limit when the model is sent them.
func TestLoopAnswersFitMaxResultBytes(t *testing.T) {
	call := completion.ToolCall{ID: "c1", Type: "function"}
	call.Function.Name = "write_file"
	started := completion.ToolCall{ID: "c2", Type: "function"}
	started.Function.Name = "write_file"
	records := []record{
		{Type: recordAssistant, Response: 1, Calls: []completion.ToolCall{call, started}},
		{Type: recordToolCall, Response: 1, Call: "c2"},
	}
	for _, m := range conversation(records, "", 40) {
		if m.Role == "tool" && (len(m.Content) > 40 || !strings.Contains(m.Content, "[truncated")) {
			t.Fatalf("%s answered with %d bytes: %q", m.ToolCallID, len(m.Content), m.Content)
		}
	}
	full := conversation(records, "", defaultMaxResultBytes)
	if full[1].Content != notRunText || full[2].Content != unknownOutcomeText {
		t.Fatalf("a default limit changed the answers: %+v", full)
	}
}

func TestWorkbenchCommandCancellationRecordsUnknown(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			host := newDirectToolHost(ToolHost{Handler: ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, failure }), Tools: []ToolDefinition{{Name: workbenchRunCommand}}})
			defer host.close()
			ready, refusal := host.prepareCall("command", workbenchRunCommand, json.RawMessage(`{}`))
			if refusal != nil {
				t.Fatal(refusal)
			}
			var recorded toolOutcome
			out := host.execute(ready, "turn", func(o toolOutcome) { recorded = o })
			if !out.ran || !out.isError || !out.unknown || !recorded.unknown {
				t.Fatalf("cancelled command could have side effects: %+v / %+v", out, recorded)
			}
		})
	}
}
