//go:build !windows

package session

import (
	"path/filepath"
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
	done := make(chan ToolResult, 1)
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
