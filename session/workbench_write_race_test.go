package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkbenchWriteSymlinkSwapConfinesEdits(t *testing.T) {
	w, work, outside := testWorkspace(t)
	w.id = newID()
	live, parked := filepath.Join(work, "racing"), filepath.Join(work, "parked")
	if err := os.Mkdir(live, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "file"), "outside unchanged")
	probe := filepath.Join(work, "symlink-probe")
	symlink(t, outside, probe)
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	// Start with a deterministic swapped-link window, so this test cannot
	// pass by racing only missing parents. Then continue concurrent swaps.
	if err := os.Rename(live, parked); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, live)
	for range 3 {
		r := call(t, w, workbenchWriteFile, map[string]any{"path": "racing/file", "content": "must be refused"})
		if !r.IsError || !strings.Contains(r.Content, "path_through_symlink") {
			t.Fatalf("swapped symlink accepted: %+v", r)
		}
	}
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, live); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	errorsSeen := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(live, parked); err != nil {
				continue
			}
			// A writer may already have created a new inside parent. Windows
			// may also refuse removal while a writer holds that parent's handle.
			// Restoration below retries both forms of expected contention.
			if os.Symlink(outside, live) == nil {
				// Keep the swapped link visible across multiple writer attempts.
				time.Sleep(5 * time.Millisecond)
			}
			_ = os.Remove(live)
			// Parent creation may race restoration more than once. Keep
			// retrying this fixture operation while the writer is active.
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := os.Rename(parked, live); err == nil {
					break
				}
				if time.Now().After(deadline) {
					errorsSeen <- fmt.Errorf("fixture restoration timed out")
					return
				}
				_ = os.RemoveAll(live)
			}
		}
	}()
	for i := range 300 {
		// Either an inside replacement or a refusal is permitted during a swap.
		if i%2 == 0 {
			call(t, w, workbenchWriteFile, map[string]any{"path": "racing/file", "content": "inside replacement"})
		} else {
			call(t, w, workbenchEditFile, map[string]any{"path": "racing/file", "old": "inside", "new": "edited"})
		}
	}
	close(stop)
	<-done
	select {
	case err := <-errorsSeen:
		t.Fatal(err)
	default:
	}
	data, err := os.ReadFile(filepath.Join(outside, "file"))
	if err != nil || string(data) != "outside unchanged" {
		t.Fatalf("write escaped: %q %v", data, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".harness-workbench-") {
			t.Fatal("temporary escaped")
		}
	}
}
