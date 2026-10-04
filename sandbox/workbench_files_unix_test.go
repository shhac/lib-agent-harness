//go:build !windows

package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// A FIFO is typed from the entry itself, never opened, so listing one cannot
// block on it.
func TestWorkbenchListMarksAFIFOWithoutOpeningIt(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	testenv.SkipIfRefused(t, "making a FIFO", syscall.Mkfifo(filepath.Join(work, "pipe"), 0o600))
	done := make(chan Result, 1)
	go func() { done <- call(t, ws, workbenchListFiles, map[string]any{}) }()
	select {
	case r := <-done:
		if r.Content != "a.txt\npipe [fifo]" {
			t.Fatalf("%q", r.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listing a FIFO blocked")
	}
}

func TestWorkbenchFIFOAndSocketRefusal(t *testing.T) {
	if contentDisabled(t, workbenchReadFile) {
		return
	}
	// A writable package directory supplies a short relative socket path.
	// Keep the workspace here too so no cross-file-system rename is needed.
	socketDir, err := os.MkdirTemp(".", ".wbs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	work, err := filepath.Abs(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkspace(Config{Root: work})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	testenv.SkipIfRefused(t, "making a FIFO", syscall.Mkfifo(filepath.Join(work, "pipe"), 0600))
	socket, err := net.Listen("unix", filepath.Join(socketDir, "socket"))
	testenv.SkipIfRefused(t, "making a workspace socket", err)
	defer socket.Close()
	for _, name := range []string{"pipe", "socket"} {
		refusedWith(t, read(t, w, name), wbNotRegular)
	}
	result := call(t, w, workbenchSearchFiles, map[string]any{"pattern": "inside"})
	if !strings.Contains(result.Content, wbNotRegular+"=2") {
		t.Fatal(result.Content)
	}
	writeFile(t, filepath.Join(work, "target"), "inside")
	w.openStep = func() {
		w.openStep = nil
		if err := os.Remove(filepath.Join(work, "target")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(filepath.Join(work, "socket"), filepath.Join(work, "target")); err != nil {
			t.Error(err)
		}
	}
	refusedWith(t, read(t, w, "target"), wbNotRegular)
}

// The swap is exactly between the pre-filter and open, rather than hoping
// a concurrent writer happens to hit that window.
func TestWorkbenchNonblockingFIFOOpenAfterLstat(t *testing.T) {
	if contentDisabled(t, workbenchReadFile) {
		return
	}
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			w, work, _ := testWorkspace(t)
			name := filepath.Join(work, "target")
			target := "target"
			if directory {
				writeFile(t, filepath.Join(name, "child"), "inside")
				target += "/child"
			} else {
				writeFile(t, name, "inside")
			}
			pipe := filepath.Join(work, "pipe")
			testenv.SkipIfRefused(t, "making a FIFO", syscall.Mkfifo(pipe, 0600))
			w.openStep = func() {
				w.openStep = nil
				if err := os.Rename(name, name+"-saved"); err != nil {
					t.Error(err)
					return
				}
				if err := os.Rename(pipe, name); err != nil {
					t.Error(err)
				}
			}
			done := make(chan workspaceAnswer, 1)
			go func() {
				raw, _ := json.Marshal(map[string]any{"path": target})
				r, e := w.readFile(context.Background(), raw)
				done <- workspaceAnswer{r, e}
			}()
			select {
			case answer := <-done:
				if answer.err != nil || !answer.result.IsError {
					t.Fatalf("%+v", answer)
				}
				if !strings.Contains(answer.result.Content, wbNotRegular) {
					t.Fatal(answer.result.Content)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("FIFO open blocked")
			}
		})
	}
}
