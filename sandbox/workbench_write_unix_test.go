//go:build !windows

package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWorkbenchWriteFIFOAndSetID(t *testing.T) {
	w, work, _ := testWorkspace(t)
	w.id = newID()
	fifo := filepath.Join(work, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() {
		r, _ := w.writeFile(context.Background(), workbenchWriteFile, json.RawMessage(`{"path":"fifo","content":"bad"}`))
		done <- r
	}()
	select {
	case r := <-done:
		if !strings.Contains(r.Content, wbNotRegular) {
			t.Fatal(r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO write blocked")
	}
	file := filepath.Join(work, "a.txt")
	if err := os.Chmod(file, 0755|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	r := call(t, w, workbenchEditFile, map[string]any{"path": "a.txt", "old": "inside", "new": "new"})
	if r.IsError {
		t.Fatal(r)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm() != 0755 {
		t.Fatalf("%v %v", info, err)
	}
}

func TestWorkbenchNewFileModeUmask(t *testing.T) {
	if mask := os.Getenv("WORKBENCH_TEST_UMASK"); mask != "" {
		n, _ := strconv.ParseInt(mask, 8, 32)
		syscall.Umask(int(n))
		w, work, _ := testWorkspace(t)
		w.id = newID()
		w.mode = 0644
		observed := false
		w.writeFault = func(stage string) error {
			if stage == "chmod" {
				entries, _ := os.ReadDir(filepath.Join(work, "nested"))
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".harness-workbench-") {
						info, _ := entry.Info()
						if info.Mode().Perm() != 0600 {
							t.Fatalf("temporary: %s", info.Mode())
						}
						observed = true
					}
				}
			}
			return nil
		}
		r := call(t, w, workbenchWriteFile, map[string]any{"path": "nested/file", "content": "content"})
		if r.IsError || !observed {
			t.Fatal(r)
		}
		info, _ := os.Stat(filepath.Join(work, "nested", "file"))
		if info.Mode().Perm() != 0644 {
			t.Fatal(info.Mode())
		}
		dir, _ := os.Stat(filepath.Join(work, "nested"))
		if dir.Mode().Perm() != 0755 {
			t.Fatal(dir.Mode())
		}
		return
	}
	for _, mask := range []string{"000", "022", "077"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWorkbenchNewFileModeUmask$")
		cmd.Env = append(os.Environ(), "WORKBENCH_TEST_UMASK="+mask)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("umask %s: %v %s", mask, err, out)
		}
	}
}
