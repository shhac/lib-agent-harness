package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestWorkspaceWorkerWaitsForCancellationSettlement(t *testing.T) {
	w, _, _ := testWorkspace(t)
	w.grace = time.Second
	entered := make(chan struct{})
	release := make(chan struct{})
	w.step = func() { close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := w.dispatch(ctx, func() (Result, error) { return w.readFile(ctx, json.RawMessage(`{"path":"a.txt"}`)) })
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("settled before worker returned: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if w.handles.Load() != 0 {
		t.Fatal(w.handles.Load())
	}
}

func TestWorkspaceWorkerRepeatedCancellation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	w, _, _ := testWorkspace(t)
	for i := 0; i < 100; i++ {
		entered, release := make(chan struct{}), make(chan struct{})
		w.step = func() { close(entered); <-release }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := w.dispatch(ctx, func() (Result, error) { return w.readFile(ctx, json.RawMessage(`{"path":"a.txt"}`)) })
			done <- err
		}()
		<-entered
		cancel()
		close(release)
		if err := <-done; err != context.Canceled {
			t.Fatal(err)
		}
		if w.handles.Load() != 0 {
			t.Fatal(w.handles.Load())
		}
	}
	w.close()
	select {
	case <-w.stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit")
	}
	awaitGoroutineBaseline(t, baseline)
}

func awaitGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline: %d, want at most %d", runtime.NumGoroutine(), baseline)
}
