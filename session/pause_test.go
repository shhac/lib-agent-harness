//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A paused channel admits nothing. Cancelling only what happened to be running
// left the queue behind it to execute afterwards — a write arriving after the
// work had been reported as stopped.
func TestPausedChannelAdmitsNothing(t *testing.T) {
	var calls atomic.Int32
	h := testHost(t, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		calls.Add(1)
		return ToolResult{Content: "wrote after pause"}, nil
	}))
	s := &Session{tools: h}
	s.CancelTools()
	c := dial(t, h, string(h.secret))
	text, isError := c.call(t, "read_file", map[string]any{})
	if !isError || calls.Load() != 0 {
		t.Fatalf("a paused channel admitted %d executions; isError=%v %q", calls.Load(), isError, text)
	}
	if !s.ToolsSettled() {
		t.Error("a paused channel with nothing admitted reported unsettled work")
	}
	// It reopens only for new work, and then it works again.
	s.ResumeTools()
	if _, isError = c.call(t, "read_file", map[string]any{}); isError {
		t.Fatal("a resumed channel still refused work")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one execution after resuming, got %d", calls.Load())
	}
}

// Work queued behind a running call is this session's responsibility too: it
// must be cancellable, must count as unsettled, and must not run after a pause.
func TestQueuedCallIsCancelledAndCountedAsUnsettled(t *testing.T) {
	running := make(chan struct{})
	release := make(chan struct{})
	var executed atomic.Int32
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		executed.Add(1)
		if c.Name == "read_file" {
			close(running)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return ToolResult{Content: "ok"}, nil
	}),
		ToolDefinition{Name: "read_file", Schema: map[string]any{"type": "object"}},
		ToolDefinition{Name: "write_file", Schema: map[string]any{"type": "object"}},
	)
	first := dial(t, h, string(h.secret))
	go func() {
		_ = first.post("tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{}})
	}()
	<-running
	queued := dial(t, h, string(h.secret))
	go func() {
		_ = queued.post("tools/call", map[string]any{"name": "write_file", "arguments": map[string]any{}})
	}()
	// Give the second call time to be admitted and start waiting on the gate.
	time.Sleep(100 * time.Millisecond)
	s := &Session{tools: h}
	if s.ToolsSettled() {
		t.Fatal("a queued call was not counted as unsettled work")
	}
	s.CancelTools()
	close(release)
	// Neither the queued call nor anything after it may execute.
	if _, isError := readResult(t, queued.receive(t)); !isError {
		t.Error("a queued call ran after the channel was paused")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !s.ToolsSettled() {
		if time.Now().After(deadline) {
			t.Fatal("the channel never settled after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := executed.Load(); got != 1 {
		t.Fatalf("expected only the already-running call to execute, got %d", got)
	}
}

// A cancellation names a request within one connection. Two harness connections
// can use the same identifier, and one must not withdraw the other's work.
func TestCancellationIsScopedToItsConnection(t *testing.T) {
	running := make(chan struct{}, 2)
	var cancelled atomic.Int32
	h := testHost(t, ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		running <- struct{}{}
		<-ctx.Done()
		cancelled.Add(1)
		return ToolResult{}, ctx.Err()
	}))
	victim := dial(t, h, string(h.secret))
	go func() {
		_ = victim.post("tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{}})
	}()
	<-running
	// A different connection withdraws request 1 — its own, not the other's.
	other := dial(t, h, string(h.secret))
	notice, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 1},
	})
	if _, err := other.conn.Write(append(notice, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(300 * time.Millisecond):
	case <-running:
		t.Fatal("unexpected second execution")
	}
	if cancelled.Load() != 0 {
		t.Fatal("one connection's cancellation withdrew another connection's call")
	}
}

// A worker that logs out stays logged out: a refresh held by a worker must not
// recreate an account the operator deliberately removed.
func TestRefreshCannotUndoAnOwnerLogout(t *testing.T) {
	source, runtime := privateDir(t), privateDir(t)
	putSynthetic(t, source, codexCredentialFile, "synthetic-login")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	putSynthetic(t, runtime, codexCredentialFile, "synthetic-refreshed")
	if err := os.Remove(filepath.Join(source, codexCredentialFile)); err != nil {
		t.Fatal(err)
	}
	if err := writeBackCredential(source, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, codexCredentialFile)); !os.IsNotExist(err) {
		t.Fatal("a worker recreated a login after the owner logged out")
	}
}

// A case-insensitive filesystem answers to more than one spelling of a path.
// Comparing text let an alias of the operator's own home through as a runtime
// home, and its configuration was replaced.
func TestCaseAliasCannotReplaceSourceConfiguration(t *testing.T) {
	source := privateDir(t)
	alias := strings.ToUpper(source)
	if _, err := os.Stat(alias); err != nil {
		t.Skip("filesystem is case sensitive")
	}
	original := "# owner's config\n"
	putSynthetic(t, source, codexConfigFile, original)
	putSynthetic(t, source, codexCredentialFile, "synthetic-login")
	if _, err := prepareRuntimeHome(source, alias); err == nil {
		t.Error("an alias of the source home was accepted as a separate runtime home")
	}
	raw, err := os.ReadFile(filepath.Join(source, codexConfigFile))
	if err != nil || string(raw) != original {
		t.Fatal("owner configuration was replaced through a case alias")
	}
}

// A composed steer replaces the turn. The replacement must outlive the bounded
// control request that asked for it — binding it to that context ended the
// session the moment steering returned.
func TestComposedSteerReplacementOutlivesItsControlRequest(t *testing.T) {
	s, w := fakeSession(t, Claude)
	s.lifetime = context.Background()
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"initial"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range turn.Events() {
		}
	}()
	w.requestFn = func(method string, _ map[string]any) (json.RawMessage, error) {
		if method == "interrupt" {
			go finishClaude(s, true)
		}
		return json.RawMessage(`{}`), nil
	}
	// A bounded control context, as any caller with a timeout would use.
	control, cancel := context.WithTimeout(ctx, 3*time.Second)
	result, err := s.Steer(control, turn.ID(), Input{"do this instead"}, SteerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Turn == nil || result.Strategy != Composed {
		t.Fatalf("unexpected steer result: %+v", result)
	}
	replacement := result.Turn
	// The control request is over; the replacement turn is not.
	cancel()
	time.Sleep(100 * time.Millisecond)
	if s.Health().State == Exited || s.Health().State == Failed {
		t.Fatalf("the session died with its control request: %+v", s.Health())
	}
	// And it still completes normally.
	go func() {
		for range replacement.Events() {
		}
	}()
	finishClaude(s, false)
	if _, err = replacement.Wait(ctx); err != nil {
		t.Fatalf("the replacement turn did not complete: %v", err)
	}
}
