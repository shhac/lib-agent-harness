//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A call withdrawn before it ever reached the gate must not run because the gate
// happened to be free. A select offered both a ready gate and an already-
// cancelled context picks either one, so testing only the gate's readiness lets
// about half of them through — which is a write executing after the turn that
// asked for it was interrupted.
func TestWithdrawnCallNeverAcquiresAFreeGate(t *testing.T) {
	h := testHost(t, echoHandler(t))
	admitted := 0
	for i := 0; i < 64; i++ {
		call, refusal := h.admit(strconv.Itoa(i), "read_file")
		if refusal != nil {
			t.Fatal("admission refused an ordinary call")
		}
		call.cancel()
		if h.acquire(call, "read_file") == nil {
			admitted++
			h.release()
		}
		h.retire(call)
	}
	if admitted > 0 {
		t.Fatalf("%d withdrawn calls ran through a free gate", admitted)
	}
}

// A harness cancels the call it just sent. The connection has to have registered
// that call before it reads the next frame, or the cancellation names something
// that does not exist yet and the call runs regardless.
func TestCancellationArrivingImmediatelyAfterTheCallIsNotLost(t *testing.T) {
	blocking := make(chan struct{})
	release := make(chan struct{})
	var ran atomic.Bool
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		if c.Name == "run_command" {
			close(blocking)
			<-release
			return ToolResult{Content: "held"}, nil
		}
		ran.Store(true)
		return ToolResult{Content: "executed"}, nil
	}),
		ToolDefinition{Name: "read_file", Schema: map[string]any{"type": "object"}},
		ToolDefinition{Name: "run_command", Schema: map[string]any{"type": "object"}},
	)
	c := dial(t, h, string(h.secret))
	c.send(t, "tools/call", map[string]any{"name": "run_command", "arguments": map[string]any{}})
	<-blocking
	// One write carrying both frames, so the cancellation is available to be read
	// the instant the call has been. This is the window that matters.
	call, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 99, "method": "tools/call",
		"params": map[string]any{"name": "read_file", "arguments": map[string]any{}},
	})
	cancel, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 99, "reason": "turn interrupted"},
	})
	if _, err := c.conn.Write(append(append(call, '\n'), append(cancel, '\n')...)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		text, isError := readResult(t, c.receive(t))
		if isError && !strings.Contains(text, "withdrawn") {
			t.Errorf("unexpected refusal: %q", text)
		}
	}
	if ran.Load() {
		t.Fatal("a call cancelled in the frame after it was sent executed anyway")
	}
}

// Cancelling asks a handler to stop; it does not establish that it has stopped.
// Nothing new may be authorized in between, and settlement must not be reported
// until the handler has actually returned.
func TestNoTurnStartsWhileCancelledToolsAreStillRunning(t *testing.T) {
	running := make(chan struct{})
	release := make(chan struct{})
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		close(running)
		<-ctx.Done()
		// A handler still finishing a write when its context ended. What it did is
		// unknown until it returns, which is what settlement is about.
		<-release
		return ToolResult{}, ctx.Err()
	}))
	c := dial(t, h, string(h.secret))
	go func() { _ = c.post("tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{}}) }()
	<-running
	s := &Session{tools: h}
	s.CancelTools()

	if err := h.readyForWork(); !errors.Is(err, ErrToolsUnsettled) {
		t.Fatalf("work was authorized over a cancelled but unfinished call: %v", err)
	}
	bounded, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stop()
	if err := s.AwaitToolsSettled(bounded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("settlement was reported before the handler returned: %v", err)
	}
	close(release)
	settling, stopSettling := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopSettling()
	if err := s.AwaitToolsSettled(settling); err != nil {
		t.Fatalf("settlement never arrived after the handler returned: %v", err)
	}
	if err := h.readyForWork(); err != nil {
		t.Fatalf("a settled channel still refused work: %v", err)
	}
	if _, isError := readResult(t, c.receive(t)); !isError {
		t.Error("a cancelled call reported success")
	}
}

// A turn ending closes tool admission, in the turn, not whenever the caller
// gets around to noticing. Both installed harnesses have been observed sending
// a tool call after their terminal result, and a caller reacting to the terminal
// event is always a little behind it — so a write would land after the work was
// declared stopped. Anything already running is left to finish.
func TestATurnEndingClosesToolAdmissionWithoutCancellingWhatRuns(t *testing.T) {
	release := make(chan struct{})
	running := make(chan struct{})
	var cancelled atomic.Bool
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		if c.Name != "run_command" {
			return ToolResult{Content: "executed"}, nil
		}
		close(running)
		select {
		case <-release:
		case <-ctx.Done():
			cancelled.Store(true)
		}
		return ToolResult{Content: "finished on its own"}, nil
	}),
		ToolDefinition{Name: "read_file", Schema: map[string]any{"type": "object"}},
		ToolDefinition{Name: "run_command", Schema: map[string]any{"type": "object"}},
	)
	turn := &Turn{id: "turn-one", events: make(chan Event, 4), done: make(chan struct{}), closeTools: h.closeAdmission}
	c := dial(t, h, string(h.secret))
	c.send(t, "tools/call", map[string]any{"name": "run_command", "arguments": map[string]any{}})
	<-running

	turn.finish("completed", nil)

	// A call sent just after the terminal result finds the channel closed.
	late := dial(t, h, string(h.secret))
	text, isError := late.call(t, "read_file", map[string]any{})
	if !isError || !strings.Contains(text, "paused") {
		t.Fatalf("a call after the turn ended was admitted: %q", text)
	}
	// The one that was already running was not abandoned half-done.
	if cancelled.Load() {
		t.Fatal("a running call was cancelled by its turn ending")
	}
	close(release)
	if got, isError := readResult(t, c.receive(t)); isError || !strings.Contains(got, "finished on its own") {
		t.Fatalf("the running call did not finish: %q", got)
	}
}

// A paused channel with nothing outstanding is still paused. Settlement is about
// what is running, not about whether new work is allowed, and confusing the two
// would let a call admitted after an interrupt look like part of the old turn.
func TestAPausedChannelStaysClosedWithNothingOutstanding(t *testing.T) {
	h := testHost(t, echoHandler(t))
	s := &Session{tools: h}
	s.CancelTools()
	if !s.ToolsSettled() {
		t.Fatal("an idle channel reported outstanding work")
	}
	c := dial(t, h, string(h.secret))
	text, isError := c.call(t, "read_file", map[string]any{})
	if !isError || !strings.Contains(text, "paused") {
		t.Fatalf("a paused channel admitted a call: %q", text)
	}
}
