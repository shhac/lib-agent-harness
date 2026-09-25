//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// contextRecorder is a caller's context handler that remembers what it was
// asked for, and can be told to fail.
type contextRecorder struct {
	mu      sync.Mutex
	reasons []ContextReason
	fail    error
	text    string
}

func (r *contextRecorder) handler(_ context.Context, reason ContextReason) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
	if r.fail != nil {
		return "", r.fail
	}
	return r.text, nil
}

func (r *contextRecorder) asked() []ContextReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ContextReason(nil), r.reasons...)
}

func contextOptions(t *testing.T, engine Engine) (Options, string, *contextRecorder) {
	t.Helper()
	o, log := persistentOptions(t, engine)
	recorder := &contextRecorder{text: "the overview"}
	o.Context = recorder.handler
	return o, log, recorder
}

func delivered(reason ContextReason, text, input string) string {
	return `<caller-context reason="` + string(reason) + "\">\n" + text + "\n</caller-context>\n\n" + input
}

// inputs is what the harness actually received as each turn's text.
func inputs(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, raw := range logged(t, log, "input:") {
		text, err := strconv.Unquote(raw)
		if err != nil {
			t.Fatalf("unreadable logged input %s", raw)
		}
		out = append(out, text)
	}
	return out
}

func equalStrings[T ~string](got, want []T) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFreshConversationDeliversStartedContextOnce(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log, recorder := contextOptions(t, engine)
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release(t, ctx, s)
			runTurn(t, ctx, s, "hello")
			runTurn(t, ctx, s, "again")
			want := []string{delivered(ContextStarted, "the overview", "hello"), "again"}
			if got := inputs(t, log); !equalStrings(got, want) {
				t.Fatalf("harness received %q, want %q", got, want)
			}
			if got := recorder.asked(); !equalStrings(got, []ContextReason{ContextStarted}) {
				t.Fatalf("caller was asked for %v", got)
			}
		})
	}
}

func TestResumedConversationAsksForNoContext(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log, recorder := contextOptions(t, engine)
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			runTurn(t, ctx, s, "hello")
			ref := s.Ref()
			release(t, ctx, s)
			resumed, opened, err := Open(ctx, o, &ref)
			if err != nil || !opened.Resumed {
				t.Fatalf("did not resume: %+v %v", opened, err)
			}
			defer release(t, ctx, resumed)
			runTurn(t, ctx, resumed, "after restart")
			if got := inputs(t, log); got[len(got)-1] != "after restart" {
				t.Fatalf("a resumed conversation was sent context: %q", got[len(got)-1])
			}
			if got := recorder.asked(); len(got) != 1 {
				t.Fatalf("caller was asked for %v", got)
			}
		})
	}
}

// A compaction during a turn is delivered with the next turn, once.
func TestCompactionDeliversCompactedContextOnTheNextTurn(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log, recorder := contextOptions(t, engine)
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release(t, ctx, s)
			runTurn(t, ctx, s, "hello")
			runTurn(t, ctx, s, "work "+fakeCompactCue)
			runTurn(t, ctx, s, "next")
			runTurn(t, ctx, s, "last")
			want := []string{delivered(ContextStarted, "the overview", "hello"), "work " + fakeCompactCue, delivered(ContextCompacted, "the overview", "next"), "last"}
			if got := inputs(t, log); !equalStrings(got, want) {
				t.Fatalf("harness received %q, want %q", got, want)
			}
			if got := recorder.asked(); !equalStrings(got, []ContextReason{ContextStarted, ContextCompacted}) {
				t.Fatalf("caller was asked for %v", got)
			}
		})
	}
}

// Codex's own compaction turn marks the conversation as compacted too.
func TestCodexCompactTurnDeliversCompactedContext(t *testing.T) {
	o, log, _ := contextOptions(t, Codex)
	ctx := probeContext(t)
	s, _, err := Open(ctx, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, ctx, s)
	runTurn(t, ctx, s, "hello")
	turn, err := s.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if result, err := turn.Wait(ctx); err != nil || result.Status != "completed" {
		t.Fatalf("compaction did not complete: %+v %v", result, err)
	}
	runTurn(t, ctx, s, "next")
	if got := inputs(t, log); got[len(got)-1] != delivered(ContextCompacted, "the overview", "next") {
		t.Fatalf("the turn after compaction carried %q", got[len(got)-1])
	}
}

// A compaction the process never got to deliver is still due after a restart.
func TestCompactionMarkerSurvivesARestart(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log, recorder := contextOptions(t, engine)
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			runTurn(t, ctx, s, "hello "+fakeCompactCue)
			ref := s.Ref()
			release(t, ctx, s)
			marker, err := os.ReadFile(filepath.Join(o.Restriction.Tools.Dir, "context.pending"))
			if err != nil || string(marker) != string(ContextCompacted) {
				t.Fatalf("the compaction was not kept on disk: %q %v", marker, err)
			}
			info, err := os.Stat(filepath.Join(o.Restriction.Tools.Dir, "context.pending"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("the marker is not owner-only: %v %v", info.Mode(), err)
			}
			resumed, opened, err := Open(ctx, o, &ref)
			if err != nil || !opened.Resumed {
				t.Fatalf("did not resume: %+v %v", opened, err)
			}
			defer release(t, ctx, resumed)
			runTurn(t, ctx, resumed, "after restart")
			runTurn(t, ctx, resumed, "then")
			got := inputs(t, log)
			if got[1] != delivered(ContextCompacted, "the overview", "after restart") || got[2] != "then" {
				t.Fatalf("harness received %q", got)
			}
			if asked := recorder.asked(); !equalStrings(asked, []ContextReason{ContextStarted, ContextCompacted}) {
				t.Fatalf("caller was asked for %v", asked)
			}
			if _, err = os.Stat(filepath.Join(o.Restriction.Tools.Dir, "context.pending")); !os.IsNotExist(err) {
				t.Fatal("a delivered marker was left on disk")
			}
		})
	}
}

// A caller that cannot supply its context stops the turn, and the reason stays
// due for the turn that follows.
func TestFailingContextFailsTheTurnAndKeepsTheMarker(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log, recorder := contextOptions(t, engine)
			refused := errors.New("overview unavailable")
			recorder.fail = refused
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release(t, ctx, s)
			if turn, err := s.StartTurn(ctx, Input{"hello"}); turn != nil || !errors.Is(err, refused) {
				t.Fatalf("a failing context handler did not fail the turn: %v", err)
			}
			if got := inputs(t, log); len(got) != 0 {
				t.Fatalf("the harness was sent %q", got)
			}
			recorder.mu.Lock()
			recorder.fail = nil
			recorder.mu.Unlock()
			runTurn(t, ctx, s, "hello")
			if got := inputs(t, log); !equalStrings(got, []string{delivered(ContextStarted, "the overview", "hello")}) {
				t.Fatalf("harness received %q", got)
			}
		})
	}
}

// Empty context sends the input as it is, and still settles the reason.
func TestEmptyContextSendsTheInputUnchanged(t *testing.T) {
	o, log, recorder := contextOptions(t, Claude)
	recorder.text = ""
	ctx := probeContext(t)
	s, _, err := Open(ctx, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, ctx, s)
	runTurn(t, ctx, s, "hello")
	runTurn(t, ctx, s, "again")
	if got := inputs(t, log); !equalStrings(got, []string{"hello", "again"}) {
		t.Fatalf("harness received %q", got)
	}
	if got := recorder.asked(); len(got) != 1 {
		t.Fatalf("caller was asked for %v", got)
	}
}

// The installed CLI nests the trigger, as compact_metadata.trigger.
func TestClaudeCompactionReadsTheNestedTrigger(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"work"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"system","subtype":"compact_boundary","session_id":"session-1","uuid":"b1","compact_metadata":{"trigger":"manual","pre_tokens":5}}`)
	event := <-turn.Events()
	for event.Kind == "context" {
		event = <-turn.Events()
	}
	if event.Kind != "compaction_completed" || event.Status != "manual" {
		t.Fatalf("unexpected event %+v", event)
	}
	s.mu.Lock()
	pending := s.contextPending
	s.mu.Unlock()
	if pending != ContextCompacted {
		t.Fatalf("a compaction left %q pending", pending)
	}
}
