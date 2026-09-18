//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Interrupting a native turn does not reach a caller's tools. Measured on both
// installed harnesses: the interrupt was acknowledged and a terminal
// interrupted result arrived in milliseconds while a hosted call was still
// running and its context had not been cancelled. A caller that treats the
// terminal event as a checkpoint is describing a workspace that is still moving.
func TestHostedToolsAreNotCancelledByATerminalTurnAlone(t *testing.T) {
	running := make(chan struct{})
	cancelled := make(chan struct{})
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		close(running)
		<-ctx.Done()
		close(cancelled)
		return ToolResult{}, ctx.Err()
	}))
	c := dial(t, h, string(h.secret))
	go func() { _ = c.post("tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{}}) }()
	<-running
	if h.served() {
		t.Error("a call that was never listed reported the surface as served")
	}
	// Nothing about the turn ending settles this.
	select {
	case <-cancelled:
		t.Fatal("a hosted call was cancelled without anyone asking")
	case <-time.After(100 * time.Millisecond):
	}
	// The channel says so, too: work is still in flight.
	settled := &Session{tools: h}
	if settled.ToolsSettled() {
		t.Fatal("the channel reported settled while a call was running")
	}
	settled.CancelTools()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the channel did not reach the running call")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !settled.ToolsSettled() {
		if time.Now().After(deadline) {
			t.Fatal("the channel never settled after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	text, isError := readResult(t, c.receive(t))
	if !isError || !strings.Contains(text, "effect is unknown") {
		t.Errorf("a cancelled call did not report its effect as uncertain: %q", text)
	}
}

// A harness withdraws a call with a cancellation notification. Ignoring it left
// work running after the turn that asked for it had been interrupted.
func TestHarnessCancellationNotificationStopsThatCall(t *testing.T) {
	running := make(chan struct{})
	cancelled := make(chan struct{})
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		close(running)
		<-ctx.Done()
		close(cancelled)
		return ToolResult{}, ctx.Err()
	}))
	c := dial(t, h, string(h.secret))
	go func() { _ = c.post("tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{}}) }()
	<-running
	notice, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 1, "reason": "turn interrupted"},
	})
	if _, err := c.conn.Write(append(notice, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("a cancellation notification did not reach the running call")
	}
}

// A call belongs to the turn that asked for it, not to whichever turn happens to
// be running when it reaches the handler. Mislabelling it would attribute work
// to the wrong part of the conversation and, after a composed steer, to a turn
// that never requested it.
func TestQueuedCallKeepsTheTurnThatRequestedIt(t *testing.T) {
	first := make(chan struct{})
	release := make(chan struct{})
	seen := make(chan string, 4)
	h := testHost(t,
		ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
			seen <- c.Name + "@" + c.TurnID
			if c.Name == "read_file" {
				close(first)
				<-release
			}
			return ToolResult{Content: "ok"}, nil
		}),
		ToolDefinition{Name: "read_file", Schema: map[string]any{"type": "object"}},
		ToolDefinition{Name: "run_command", Schema: map[string]any{"type": "object"}},
	)
	var turnMu sync.Mutex
	turn := "turn-one"
	h.activeTurn = func() string { turnMu.Lock(); defer turnMu.Unlock(); return turn }
	c := dial(t, h, string(h.secret))
	go func() { _ = c.post("tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{}}) }()
	<-first
	// A second call arrives while the first is still running, then the session
	// moves on to another turn before the queue drains.
	queued := dial(t, h, string(h.secret))
	go func() {
		_ = queued.post("tools/call", map[string]any{"name": "run_command", "arguments": map[string]any{}})
	}()
	time.Sleep(50 * time.Millisecond)
	turnMu.Lock()
	turn = "turn-two"
	turnMu.Unlock()
	close(release)
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case entry := <-seen:
			got[entry] = true
		case <-time.After(3 * time.Second):
			t.Fatal("a queued call never ran")
		}
	}
	if !got["read_file@turn-one"] {
		t.Errorf("the first call lost its turn: %v", got)
	}
	if !got["run_command@turn-one"] {
		t.Errorf("a call requested during turn-one was attributed elsewhere: %v", got)
	}
}
