package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// startedCodexTurn opens a turn whose server-assigned id is turn-1, so the
// routing guards below have a concrete turn to be measured against.
func startedCodexTurn(t *testing.T) (*Session, *fakeWire, *Turn) {
	t.Helper()
	s, w := fakeSession(t, Codex)
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
	}
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	return s, w, turn
}

// A frame addressed to another thread or another turn is not this turn's
// answer. Accepting one would let a stale or crossed notification overwrite
// the text a caller is about to read.
func TestCodexIgnoresForeignThreadAndTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"foreign thread", `{"method":"item/agentMessage/delta","params":{"threadId":"other-session","turnId":"turn-1","itemId":"msg","delta":"intruder"}}`},
		{"foreign turn", `{"method":"item/agentMessage/delta","params":{"threadId":"session-1","turnId":"turn-99","itemId":"msg","delta":"intruder"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, turn := startedCodexTurn(t)
			ctx := testContext(t)
			notify(s, tc.frame)
			notify(s, `{"method":"item/agentMessage/delta","params":{"threadId":"session-1","turnId":"turn-1","itemId":"msg","delta":"mine"}}`)
			notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
			result, err := turn.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != "mine" {
				t.Fatalf("foreign frame reached this turn: %q", result.Text)
			}
		})
	}
}

// A Claude frame naming a different session is a protocol violation, not a
// frame to skip: the transport is talking about a conversation we did not open.
func TestClaudeForeignSessionFailsProtocol(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"other-session","message":{"id":"a","content":[{"type":"text","text":"intruder"}]}}`)
	if _, err := turn.Wait(ctx); err != ErrProtocol {
		t.Fatalf("foreign session accepted: %v", err)
	}
}

// Subagent events belong to a different native turn. They must neither become
// this turn's answer nor open a tool card this turn will never close.
func TestClaudeIgnoresSubagentEvents(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"session-1","parent_tool_use_id":"tool-7","message":{"id":"sub","content":[{"type":"text","text":"subagent text"},{"type":"tool_use","id":"nested","name":"Read"}]}}`)
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"a","content":[{"type":"text","text":"mine"}]}}`)
	// A result carrying no text of its own leaves the assistant message standing,
	// so the assertion below is about routing rather than about result replacement.
	notify(s, `{"type":"result","subtype":"success","is_error":false,"session_id":"session-1","usage":{"input_tokens":5,"output_tokens":2}}`)
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "mine" {
		t.Fatalf("subagent text reached this turn: %q", result.Text)
	}
	for event := range turn.Events() {
		if event.ItemID == "nested" {
			t.Fatal("subagent tool card opened on this turn")
		}
	}
}

// An interrupted turn's totals are not the turn's totals. Numbers reported
// alongside a failure must not be promoted to a known accounting figure.
func TestInterruptedClaudeErrorUsageStaysUnknown(t *testing.T) {
	s, w := fakeSession(t, Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		notify(s, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"session-1","usage":{"input_tokens":120,"output_tokens":45,"cache_read_input_tokens":8}}`)
		return json.RawMessage(`{}`), nil
	}
	if err := s.Interrupt(ctx, turn.ID()); err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "interrupted" {
		t.Fatalf("lost interrupt correlation: %+v", result)
	}
	if result.Usage.Known || result.Usage.Input != 0 || result.Usage.Output != 0 {
		t.Fatalf("interrupted totals reported as known usage: %+v", result.Usage)
	}
}

// A tool result closes the card its tool_use opened, and says whether the tool
// failed. A card left open would read as a tool still running.
func TestClaudeToolResultsCloseTheirCards(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"a","content":[{"type":"tool_use","id":"ok-call","name":"Read"},{"type":"tool_use","id":"bad-call","name":"Edit"}]}}`)
	notify(s, `{"type":"user","session_id":"session-1","message":{"content":[{"type":"tool_result","tool_use_id":"ok-call"},{"type":"text","text":"not a result"},{"type":"tool_result","tool_use_id":"bad-call","is_error":true}]}}`)
	finishClaude(s, false)
	if _, err = turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for event := range turn.Events() {
		if event.Kind == "tool_completed" {
			status[event.ItemID] = event.Status
		}
	}
	if len(status) != 2 || status["ok-call"] != "completed" || status["bad-call"] != "failed" {
		t.Fatalf("tool cards were not closed with their outcomes: %v", status)
	}
}

// Streamed text belongs to the message that is streaming. A new message starts
// its own text rather than appending to the last one's, which is what lets the
// final answer be the last message instead of every message run together.
func TestClaudeStreamedTextFollowsTheCurrentMessage(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"message_start","message":{"id":"first"}}}`)
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"planning "}}}`)
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"aloud"}}}`)
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}}`)
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"message_start","message":{"id":"second"}}}`)
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}}`)
	notify(s, `{"type":"result","subtype":"success","is_error":false,"session_id":"session-1","usage":{"input_tokens":1,"output_tokens":1}}`)
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "answer" {
		t.Fatalf("streamed text crossed messages: %q", result.Text)
	}
	var deltas []string
	for event := range turn.Events() {
		if event.Kind == "text_delta" {
			deltas = append(deltas, event.ItemID+":"+event.Text)
		}
	}
	if strings.Join(deltas, "|") != "first:planning |first:aloud|second:answer" {
		t.Fatalf("deltas were not attributed to their messages: %q", deltas)
	}
}
