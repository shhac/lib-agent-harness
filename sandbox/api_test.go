package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkspacePublicTools(t *testing.T) {
	root := t.TempDir()
	w, err := OpenWorkspace(Config{Root: root, SessionID: "12345678-1234-1234-1234-123456789abc", NewFileMode: 0600})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		call func(context.Context, json.RawMessage) (Result, error)
		args string
	}{
		{w.Write, `{"path":"file","content":"old"}`},
		{w.Edit, `{"path":"file","old":"old","new":"new"}`},
		{w.Read, `{"path":"file"}`},
		{w.List, `{}`},
		{w.Search, `{"pattern":"new"}`},
	} {
		r, err := tc.call(ctx, json.RawMessage(tc.args))
		if err != nil || r.IsError {
			t.Fatalf("%s: %+v %v", tc.args, r, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "file"))
	if err != nil || string(data) != "new" {
		t.Fatalf("%s %v", data, err)
	}
	if code := w.CheckDir(ctx, "."); code != "" {
		t.Fatal(code)
	}
	w.Close()
	w.Close()
	if _, err := w.Read(ctx, json.RawMessage(`{"path":"file"}`)); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestWorkspacePublicCallsSerialize(t *testing.T) {
	w, _, _ := testWorkspace(t)
	entered, release := make(chan struct{}), make(chan struct{})
	otherEntered := make(chan struct{}, 8)
	completed := make(chan struct{}, 8)
	var first atomic.Bool
	var active, maximum atomic.Int32
	w.step = func() {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		} else {
			select {
			case otherEntered <- struct{}{}:
			default:
			}
		}
	}
	var wg sync.WaitGroup
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { unblock(); wg.Wait() }()
	read := func() {
		defer wg.Done()
		r, err := w.Read(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
		if err != nil || r.IsError {
			t.Errorf("%+v %v", r, err)
		}
		completed <- struct{}{}
	}
	wg.Add(1)
	go read()
	<-entered
	var waiting sync.WaitGroup
	for range 7 {
		wg.Add(1)
		waiting.Add(1)
		go func() { waiting.Done(); read() }()
	}
	waiting.Wait()
	select {
	case <-otherEntered:
		t.Fatal("another caller entered I/O while the first was blocked")
	case <-completed:
		t.Fatal("another caller completed while the first was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	if maximum.Load() != 1 {
		t.Fatalf("concurrent I/O: maximum %d", maximum.Load())
	}
	unblock()
	wg.Wait()
	if maximum.Load() != 1 || active.Load() != 0 {
		t.Fatalf("I/O concurrency: max=%d active=%d", maximum.Load(), active.Load())
	}
	if w.handles.Load() != 0 {
		t.Fatal("handles leaked")
	}
}

func TestWorkspacePublicStuckError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "inside\n")
	failures := make(chan error, 1)
	w, err := OpenWorkspace(Config{Root: root, Grace: 20 * time.Millisecond, OnFailure: func(err error) { failures <- err }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	w.step = func() { close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := w.Read(ctx, json.RawMessage(`{"path":"a.txt"}`)); done <- err }()
	<-entered
	w.Close() // A late failure must still reach the configured callback.
	cancel()
	err = <-done
	select {
	case late := <-failures:
		if late != err {
			t.Fatalf("callback failure changed: %v != %v", late, err)
		}
	case <-time.After(time.Second):
		t.Fatal("configured late-failure callback was not called")
	}
	var failure *CommandError
	if !errors.As(err, &failure) || failure.Code != WorkspaceIOStuck || !w.Stuck() {
		t.Errorf("%v", err)
	}
	unblock()
	select {
	case <-w.stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not settle")
	}
	if w.handles.Load() != 0 {
		t.Fatal("handles leaked")
	}
}

func TestWorkspaceReservedRemovalFailure(t *testing.T) {
	w, root, _ := testWorkspace(t)
	w.id = "12345678-1234-1234-1234-123456789abc"
	own := ".harness-workbench-12345678123412341234123456789abc-0123456789abcdef.tmp"
	other := ".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(root, own), "partial")
	writeFile(t, filepath.Join(root, other), "other")
	w.writeFault = func(stage string) error {
		if stage == "remove_reserved" {
			return errors.New("injected")
		}
		return nil
	}
	if err := w.RemoveReserved(context.Background(), "a.txt", w.id); err == nil {
		t.Fatal("removal failure lost")
	}
	w.writeFault = nil
	if err := w.RemoveReserved(context.Background(), "a.txt", w.id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, own)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own temporary survived")
	}
	if data, err := os.ReadFile(filepath.Join(root, other)); err != nil || string(data) != "other" {
		t.Fatal("another session touched")
	}
}
