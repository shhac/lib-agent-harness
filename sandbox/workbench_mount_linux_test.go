package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWorkbenchBindMountAndParentSwap(t *testing.T) {
	work := os.Getenv("AGENT_HARNESS_TEST_BIND_MOUNT")
	if work == "" {
		t.Skip("bind-mount witness is supplied by Linux CI")
	}
	w, err := OpenWorkspace(Config{Root: work})
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	marker := "OUTSIDE-BIND-MOUNT-MARKER"
	readResult := call(t, w, workbenchReadFile, map[string]any{"path": "parent/mount/marker"})
	if !strings.Contains(readResult.Content, wbOtherMount) {
		t.Fatal(readResult.Content)
	}
	listing := call(t, w, workbenchListFiles, map[string]any{"depth": 8})
	if !strings.Contains(listing.Content, "parent/mount/ [mount]") {
		t.Fatal(listing.Content)
	}
	fileRead := call(t, w, workbenchReadFile, map[string]any{"path": "parent/filemount"})
	if !strings.Contains(fileRead.Content, wbOtherMount) {
		t.Fatal(fileRead.Content)
	}
	if !strings.Contains(listing.Content, "parent/filemount [mount]") {
		t.Fatal(listing.Content)
	}
	search := call(t, w, workbenchSearchFiles, map[string]any{"pattern": marker})
	if strings.Contains(search.Content, marker) || !strings.Contains(search.Content, wbOtherMount) {
		t.Fatal(search.Content)
	}
	if strings.Contains(search.Content, "directories_incomplete") {
		t.Fatal(search.Content)
	}
	parent, held := filepath.Join(work, "parent"), filepath.Join(work, "held")
	stop := make(chan struct{})
	ready := make(chan error, 1)
	faults := make(chan error, 1)
	var swaps atomic.Int32
	swap := func() (result error) {
		if err := os.Rename(parent, held); err != nil {
			return err
		}
		defer func() { result = errors.Join(result, os.RemoveAll(parent), os.Rename(held, parent)) }()
		if err := os.Mkdir(parent, 0700); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(parent, "mount"), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(parent, "mount", "marker"), []byte("inside"), 0600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(parent, "filemount"), []byte("inside"), 0600)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := swap()
		if err == nil {
			swaps.Add(1)
		}
		ready <- err
		if err != nil {
			return
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := swap(); err != nil {
				faults <- err
				return
			}
			swaps.Add(1)
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
		if swaps.Load() == 0 {
			t.Error("no mount parent swaps succeeded")
		}
		select {
		case err := <-faults:
			t.Error(err)
		default:
		}
	}()
	if err := <-ready; err != nil {
		t.Fatal("mount parent cannot be swapped:", err)
	}
	for i := 0; i < 100; i++ {
		for _, tool := range []string{workbenchReadFile, workbenchListFiles, workbenchSearchFiles} {
			args := map[string]any{"path": "parent/mount/marker", "pattern": marker}
			if tool == workbenchListFiles {
				args = map[string]any{"depth": 8}
			}
			r := call(t, w, tool, args)
			if strings.Contains(r.Content, marker) {
				t.Fatalf("%s leaked %q", tool, r.Content)
			}
			r = call(t, w, workbenchReadFile, map[string]any{"path": "parent/filemount"})
			if strings.Contains(r.Content, marker) {
				t.Fatal(r.Content)
			}
		}
	}
}
