package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

func TestWorkbenchHardLinkAndConcurrentSwaps(t *testing.T) {
	if runtime.GOOS == "windows" {
		w, work, outside := testWorkspace(t)
		name := filepath.Join(work, "changing")
		testenv.SkipIfRefused(t, "creating a hard link", os.Link(filepath.Join(outside, "secret.txt"), name))
		refusedWith(t, read(t, w, "changing"), wbLinked)
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		if r := read(t, w, "a.txt"); r.IsError || !strings.Contains(r.Content, "inside") {
			t.Fatal(r)
		}
		return
	}
	w, source, observations := modeledLinkWorkspace(t)
	// These adapters model a still-reachable name with settled descriptor facts;
	// they do not claim to pause unlink inside the kernel or reproduce the CI event.
	r, err := w.readFile(context.Background(), json.RawMessage(`{"path":"changing"}`))
	data, sourceErr := os.ReadFile(source)
	if sourceErr != nil || string(data) != outsideMarker+"\n" {
		t.Fatalf("outside source changed: %q %v", data, sourceErr)
	}
	t.Logf("outside source unchanged: %q", data)

	if ToolAvailability(workbenchReadFile) != "" {
		assertContentRefusal(t, workbenchReadFile, r, err)
		if *observations != 0 {
			t.Fatal("disabled read reached namespace observations")
		}
	} else {
		t.Logf("baseline observation: content=%q error=%v", r.Content, err)
	}
	if strings.Contains(r.Content, outsideMarker) {
		t.Fatalf("read_file: %q", r.Content)
	}
	w.statName, w.openName = nil, nil
	for _, tool := range []string{workbenchReadFile, workbenchSearchFiles} {
		r := call(t, w, tool, map[string]any{"path": "a.txt", "pattern": "inside"})
		if ToolAvailability(tool) != "" {
			refusedWith(t, r, RefusedNotOffered)
		} else if !strings.Contains(r.Content, "inside") {
			t.Fatal(r)
		}
	}
	data, err = os.ReadFile(source)
	if err != nil || string(data) != outsideMarker+"\n" {
		t.Fatalf("outside source changed: %q %v", data, err)
	}
}

// modeledLinkWorkspace joins the real link/open/unlink fixture before installing
// instance-local namespace adapters. Only reachability is modeled; descriptor
// identity, mount and link-count observations are real.
func modeledLinkWorkspace(t *testing.T) (*Workspace, string, *int) {
	t.Helper()
	w, work, outside := testWorkspace(t)
	source, name := filepath.Join(outside, "secret.txt"), filepath.Join(work, "changing")
	testenv.SkipIfRefused(t, "creating a hard link", os.Link(source, name))
	// Persistent hard links remain real fixture controls.
	info, err := os.Stat(name)
	if err != nil || wsfile.LinkCount(info) != 2 {
		t.Fatalf("persistent link: %v %v", info, err)
	}
	listing := call(t, w, workbenchListFiles, map[string]any{})
	if !strings.Contains(listing.Content, "changing [linked]") {
		t.Fatal(listing)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	writeFile(t, name, "inside\n")
	ready, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var retained *os.File
	go func() {
		close(ready)
		<-proceed
		if err := os.Remove(name); err != nil {
			done <- err
			return
		}
		if err := os.Link(source, name); err != nil {
			done <- err
			return
		}
		f, err := os.Open(name)
		if err != nil {
			done <- err
			return
		}
		retained = f
		if err := os.Remove(name); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	<-ready
	// Join the swap before installing the modeled namespace observations.
	close(proceed)
	if err := <-done; err != nil {
		if retained != nil {
			retained.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { retained.Close() })
	facts, err := wsfile.Check(retained, w.mount)
	if err != nil {
		t.Fatalf("descriptor facts unavailable: %v", err)
	}
	info, err = retained.Stat()
	sourceInfo, sourceErr := os.Stat(source)
	if err != nil || sourceErr != nil || !os.SameFile(info, sourceInfo) || wsfile.LinkCount(info) != 1 || !facts.Regular || !facts.SameMount || facts.Links != 1 {
		t.Fatalf("settled descriptor: %v %v %+v", info, err, facts)
	}
	if _, err := os.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("completed unlink: %v", err)
	}
	t.Logf("modeled namespace reachability; real descriptor nlink=%d mount facts=%+v", wsfile.LinkCount(info), facts)
	observations := new(int)
	w.statName = func(*os.Root, string) (fs.FileInfo, error) { *observations++; return retained.Stat() }
	w.openName = func(*os.Root, string) (*os.File, error) { *observations++; return retained, nil }

	return w, source, observations
}

func TestWorkbenchModeledReadAdmissionBaseline(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	w, source, observations := modeledLinkWorkspace(t)
	// Exercise the retained read admission directly, beneath the unconditional
	// content-operation policy. This test-only observation offers no runtime bypass.
	top, err := w.rootNode()
	if err != nil {
		t.Fatal(err)
	}
	cursor := w.newCursor(top, readPolicy)
	defer cursor.close()
	f, code := w.openRegular(cursor, top, "changing")
	if code != "" {
		t.Fatalf("baseline admission refused: %s", code)
	}
	data, tooLarge, err := wsfile.ReadAtMost(f, maxReadFileBytes, nil)
	w.release(f)
	if err != nil || tooLarge || string(data) != outsideMarker+"\n" || *observations != 3 {
		t.Fatalf("modeled baseline: bytes=%q tooLarge=%v err=%v observations=%d", data, tooLarge, err, *observations)
	}
	outside, err := os.ReadFile(source)
	if err != nil || string(outside) != outsideMarker+"\n" {
		t.Fatalf("outside source changed: %q %v", outside, err)
	}
	t.Logf("modeled retained admission baseline: bytes=%q; outside source unchanged; platform=%s/%s", data, runtime.GOOS, runtime.GOARCH)
	// With real namespace observations, completed unlink is refused; an ordinary
	// singly linked workspace file is admitted. These are positive path controls.
	w.statName, w.openName = nil, nil
	if f, code := w.openRegular(cursor, top, "changing"); code != wbNotFound {
		if f != nil {
			w.release(f)
		}
		t.Fatalf("completed unlink: %s", code)
	}
	f, code = w.openRegular(cursor, top, "a.txt")
	if code != "" {
		t.Fatal(code)
	}
	inside, _, err := wsfile.ReadAtMost(f, maxReadFileBytes, nil)
	w.release(f)
	if err != nil || string(inside) != "inside\n" {
		t.Fatalf("ordinary control: %q %v", inside, err)
	}
}
