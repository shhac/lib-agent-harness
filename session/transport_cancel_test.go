package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockedStdin parks the first Write until it is closed, the way a real CLI
// that has stopped reading its stdin parks a frame mid-encode. Close is the
// release mechanism, and the wire's stop closes it exactly as newProcessWire's
// stop closes the process stdin.
type blockedStdin struct {
	entered chan struct{}
	release chan struct{}

	enterOnce   sync.Once
	releaseOnce sync.Once
	closed      atomic.Bool
	writes      atomic.Int32

	mu        sync.Mutex
	delivered []byte
}

func (b *blockedStdin) Write(p []byte) (int, error) {
	b.writes.Add(1)
	b.enterOnce.Do(func() { close(b.entered) })
	<-b.release
	if b.closed.Load() {
		return 0, errors.New("stdin closed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delivered = append(b.delivered, p...)
	return len(p), nil
}

func (b *blockedStdin) Close() error {
	b.closed.Store(true)
	b.releaseOnce.Do(func() { close(b.release) })
	return nil
}

func (b *blockedStdin) bytesDelivered() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.delivered)
}

// gateWatcher reports when a caller has reached send's first select, the one
// that parks on an occupied write gate. send consults ctx.Err() before that
// select and ctx.Done() only inside it, so the first Done call is the signal
// that this caller is committed to the select and is queued behind the write
// already in flight. No sleep and no scheduler assumption is involved.
type gateWatcher struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (g *gateWatcher) Done() <-chan struct{} {
	g.once.Do(func() { close(g.entered) })
	return g.Context.Done()
}

func awaitSignal(t *testing.T, signal <-chan struct{}, deadline <-chan time.Time, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-deadline:
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitSend(t *testing.T, sent <-chan error, deadline <-chan time.Time, what string) error {
	t.Helper()
	select {
	case err := <-sent:
		return err
	case <-deadline:
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// A cancelled write must not leave an ambiguous late prompt. With one frame
// parked mid-write and a second caller queued behind the write gate, cancelling
// must close the session exactly once, report both callers as undelivered, and
// write no further frame.
func TestCancelledBlockedWriteClosesSessionOnceWithQueuedSender(t *testing.T) {
	stdin := &blockedStdin{entered: make(chan struct{}), release: make(chan struct{})}
	// Installed before any wait: a failing assertion must never leave the parked
	// writer, or the goroutines queued behind it, blocked forever.
	t.Cleanup(func() { stdin.Close() })

	var stops atomic.Int32
	w := &streamWire{
		engine:    Codex,
		stdin:     stdin,
		stdout:    io.NopCloser(strings.NewReader("")),
		pending:   map[string]chan response{},
		done:      make(chan struct{}),
		writeGate: make(chan struct{}, 1),
		event:     func(map[string]json.RawMessage) {},
		ended:     func(error) {},
	}
	// Mirrors newProcessWire's stop: closing stdin releases a parked write.
	w.stop = func() { stops.Add(1); stdin.Close() }
	t.Cleanup(w.close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	deadline := time.After(30 * time.Second)

	first := make(chan error, 1)
	go func() { first <- w.send(ctx, map[string]any{"id": "1", "method": "turn/start"}) }()
	awaitSignal(t, stdin.entered, deadline, "the first frame to park inside Write")

	queued := &gateWatcher{Context: ctx, entered: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- w.send(queued, map[string]any{"id": "2", "method": "turn/start"}) }()
	awaitSignal(t, queued.entered, deadline, "the second sender to reach the occupied write gate")

	cancel()

	if err := awaitSend(t, first, deadline, "the parked write"); !errors.Is(err, context.Canceled) {
		t.Fatalf("parked write did not report cancellation: %v", err)
	}
	// The queued sender is released by whichever of the cancelled context or the
	// now-closed session it observes first; both mean the frame was not sent.
	if err := awaitSend(t, second, deadline, "the queued write"); !errors.Is(err, context.Canceled) && !errors.Is(err, ErrClosed) {
		t.Fatalf("queued write reported neither cancellation nor closure: %v", err)
	}

	if got := stops.Load(); got != 1 {
		t.Fatalf("session stopped %d times, want exactly 1", got)
	}
	if got := stdin.writes.Load(); got != 1 {
		t.Fatalf("%d frames reached Write, want only the first", got)
	}
	if got := stdin.bytesDelivered(); got != 0 {
		t.Fatalf("a cancelled frame was delivered anyway: %d bytes", got)
	}

	// A later retry must fail on the closed transport rather than write a
	// second copy of the prompt.
	if err := w.send(context.Background(), map[string]any{"id": "3", "method": "turn/start"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("send on a closed session: %v", err)
	}
	if got := stdin.writes.Load(); got != 1 {
		t.Fatalf("a frame was written after close: %d writes", got)
	}
	if got := stdin.bytesDelivered(); got != 0 {
		t.Fatalf("a frame was delivered after close: %d bytes", got)
	}
}
