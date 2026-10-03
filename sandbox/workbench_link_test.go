package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestWorkbenchHardLinkAndConcurrentSwaps(t *testing.T) {
	w, work, outside := testWorkspace(t)
	source := filepath.Join(outside, "secret.txt")
	name := filepath.Join(work, "changing")
	testenv.SkipIfRefused(t, "creating a hard link", os.Link(source, name))
	if r := read(t, w, "changing"); !strings.Contains(r.Content, wbLinked) {
		t.Fatal(r.Content)
	}
	listing := call(t, w, workbenchListFiles, map[string]any{})
	if runtime.GOOS != "windows" && !strings.Contains(listing.Content, "changing [linked]") {
		t.Fatal(listing.Content)
	}
	_ = os.Remove(name)
	// Establish the symlink privilege before starting the concurrent mutator.
	symlink(t, outside, name)
	_ = os.Remove(name)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Link(source, name)
			_ = os.Remove(name)
			_ = os.Symlink(outside, name)
			_ = os.Remove(name)
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 100; i++ {
		for _, tool := range []string{workbenchReadFile, workbenchSearchFiles} {
			r := call(t, w, tool, map[string]any{"path": "changing", "pattern": outsideMarker})
			if strings.Contains(r.Content, outsideMarker) {
				t.Fatalf("%s: %q", tool, r.Content)
			}
		}
	}
}
