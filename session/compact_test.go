package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func compactStarted(s *Session) {
	notify(s, `{"method":"turn/started","params":{"threadId":"session-1","turn":{"id":"compact-1","status":"inProgress"}}}`)
}
func compactFinished(s *Session, status string) {
	notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"compact-1","status":"`+status+`"}}}`)
}
func TestCompactWaitsForNativeCompletion(t *testing.T) {
	for _, early := range []bool{false, true} {
		s, w := fakeSession(t, Codex)
		w.requestFn = func(method string, p map[string]any) (json.RawMessage, error) {
			if method != "thread/compact/start" || p["threadId"] != "session-1" || len(p) != 1 {
				t.Fatalf("unexpected request %s %+v", method, p)
			}
			if early {
				compactStarted(s)
			}
			return json.RawMessage(`{}`), nil
		}
		turn, err := s.Compact(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-turn.done:
			t.Fatal("request ack pretended complete")
		default:
		}
		if _, err = s.StartTurn(testContext(t), Input{Text: "hi"}); !errors.Is(err, ErrBusy) {
			t.Fatalf("not busy: %v", err)
		}
		if _, err = s.Compact(testContext(t)); !errors.Is(err, ErrBusy) {
			t.Fatalf("duplicate compact: %v", err)
		}
		if !early {
			if turn.ID() != "" {
				t.Fatal("fabricated native ID")
			}
			compactStarted(s)
		}
		if turn.ID() != "compact-1" {
			t.Fatal(turn.ID())
		}
		notify(s, `{"method":"item/started","params":{"threadId":"session-1","turnId":"compact-1","item":{"id":"compact-item","type":"contextCompaction"}}}`)
		notify(s, `{"method":"item/completed","params":{"threadId":"session-1","turnId":"compact-1","item":{"id":"compact-item","type":"contextCompaction"}}}`)
		compactFinished(s, "completed")
		result, err := turn.Wait(testContext(t))
		if err != nil || result.Status != "completed" {
			t.Fatalf("%+v %v", result, err)
		}
		if s.Capabilities().Compact.Availability != Native {
			t.Fatal("not verified")
		}
		found := false
		for event := range turn.Events() {
			if event.Kind == "compaction_completed" {
				found = true
			}
		}
		if !found {
			t.Fatal("missing native compaction event")
		}
	}
}
func TestCompactUnsupportedFailedAndCancelled(t *testing.T) {
	s, w := fakeSession(t, Claude)
	if _, err := s.Compact(testContext(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if len(w.calls) != 0 {
		t.Fatal("Claude sent a fake compaction prompt")
	}
	s, w = fakeSession(t, Codex)
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) { return nil, ErrUnsupported }
	if _, err := s.Compact(testContext(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if s.Capabilities().Compact.Availability != Unsupported {
		t.Fatal("capability not updated")
	}
	s, _ = fakeSession(t, Codex)
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := s.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = turn.Wait(testContext(t)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s, _ = fakeSession(t, Codex)
	turn, err = s.Compact(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	compactStarted(s)
	compactFinished(s, "failed")
	if _, err = turn.Wait(testContext(t)); !errors.Is(err, ErrTurnFailed) {
		t.Fatal(err)
	}
}
