package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestWorkbenchAtomicWriteAndEdit(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	w, work, _ := testWorkspace(t)
	w.id = newID()
	w.mode = 0644
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{workbenchWriteFile, map[string]any{"path": "new/dir/file", "content": "one one"}, "one one"},
		{workbenchEditFile, map[string]any{"path": "new/dir/file", "old": "one", "new": "two", "replace_all": true}, "two two"},
	} {
		if ToolAvailability(tc.tool) != "" {
			r, err := w.writeFile(context.Background(), tc.tool, mustArgs(tc.args))
			assertContentRefusal(t, tc.tool, r, err)
			continue
		}
		r := call(t, w, tc.tool, tc.args)
		if r.IsError {
			t.Fatal(r.Content)
		}
		data, e := os.ReadFile(filepath.Join(work, "new", "dir", "file"))
		if e != nil || string(data) != tc.want {
			t.Fatalf("%q %v", data, e)
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(work, "new", "dir", "file"))
		if info.Mode().Perm() != 0644 {
			t.Fatal(info.Mode())
		}
	}
	if ToolAvailability(workbenchEditFile) != "" {
		return
	}
	r := call(t, w, workbenchEditFile, map[string]any{"path": "new/dir/file", "old": "two", "new": "x"})
	if !strings.Contains(r.Content, "match_not_unique") {
		t.Fatal(r)
	}
	r = call(t, w, workbenchEditFile, map[string]any{"path": "new/dir/file", "old": "missing", "new": "x"})
	if !strings.Contains(r.Content, "match_not_found") {
		t.Fatal(r)
	}

}

func TestWorkbenchWriteRefusals(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	w, work, outside := testWorkspace(t)
	w.id = newID()
	writeFile(t, filepath.Join(work, ".git", "config"), "metadata")
	reserved := ".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(work, reserved), "another session's temporary")
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(work, "link"))
	symlink(t, outside, filepath.Join(work, "dirlink"))
	if err := os.Chmod(filepath.Join(work, "a.txt"), 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(work, "a.txt"), 0600)
	for _, tc := range []struct{ path, code string }{{"link", wbIsSymlink}, {"dirlink/file", wbThroughSymlink}, {".git/config", wbReserved}, {".GIT/config", wbReserved}, {".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp", wbReserved}, {"a.txt", "file_read_only"}, {"../outside/secret.txt", wbOutside}, {"/absolute", wbPathInvalid}} {
		r := call(t, w, workbenchWriteFile, map[string]any{"path": tc.path, "content": "bad"})
		if !r.IsError || !strings.Contains(r.Content, tc.code) {
			t.Fatalf("%s: %+v", tc.path, r)
		}
	}
	data, _ := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if string(data) != outsideMarker+"\n" {
		t.Fatal("outside changed")
	}
	data, err := os.ReadFile(filepath.Join(work, reserved))
	if err != nil || string(data) != "another session's temporary" {
		t.Fatal("reserved target changed")
	}
	r := call(t, w, workbenchEditFile, map[string]any{"path": reserved, "old": "another", "new": "bad"})
	if ToolAvailability(workbenchEditFile) != "" {
		refusedWith(t, r, RefusedNotOffered)
		return
	}
	if !r.IsError || !strings.Contains(r.Content, wbReserved) {
		t.Fatal(r)
	}
}

func TestWorkbenchNewReadOnlyModeCreatesParents(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	if runtime.GOOS == "windows" {
		t.Skip("Windows inherits ACLs instead of permission bits")
	}
	w, work, _ := testWorkspace(t)
	w.id = newID()
	w.mode = 0400
	r := call(t, w, workbenchWriteFile, map[string]any{"path": "parent/file", "content": "content"})
	defer os.Chmod(filepath.Join(work, "parent"), 0700)
	if r.IsError {
		t.Fatal(r)
	}
	info, err := os.Stat(filepath.Join(work, "parent"))
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("%v %v", info, err)
	}
	info, err = os.Stat(filepath.Join(work, "parent", "file"))
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("%v %v", info, err)
	}
}

func TestWorkbenchWriteFaults(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	for _, stage := range []string{"create", "write", "sync_file", "chmod", "rename", "sync_directory"} {
		t.Run(stage, func(t *testing.T) {
			w, work, _ := testWorkspace(t)
			w.id = newID()
			w.writeFault = func(s string) error {
				if s == stage {
					return errors.New("injected")
				}
				return nil
			}
			r := call(t, w, workbenchWriteFile, map[string]any{"path": "a.txt", "content": "replacement"})
			data, _ := os.ReadFile(filepath.Join(work, "a.txt"))
			if stage == "sync_directory" {
				if !strings.Contains(r.Content, wbWriteUnknown) || string(data) != "replacement" {
					t.Fatalf("%+v %s", r, data)
				}
			} else if !strings.Contains(r.Content, wbWriteFailed) || string(data) != "inside\n" {
				t.Fatalf("%+v %s", r, data)
			}
			entries, _ := os.ReadDir(work)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".harness-workbench-") {
					t.Fatalf("temporary remains: %s", e.Name())
				}
			}
		})
	}
}

func TestWorkbenchWritePreservesModeAndRefusesLinks(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	for _, mode := range []os.FileMode{0755, 0640} {
		t.Run(mode.String(), func(t *testing.T) {
			w, work, _ := testWorkspace(t)
			w.id = newID()
			file := filepath.Join(work, "a.txt")
			if os.Chmod(file, mode) != nil {
				t.Fatal("chmod")
			}
			r := call(t, w, workbenchWriteFile, map[string]any{"path": "a.txt", "content": "edited"})
			if r.IsError {
				t.Fatal(r)
			}
			info, _ := os.Stat(file)
			if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
				t.Fatal(info.Mode())
			}
			if err := os.Link(file, filepath.Join(work, "hard")); err != nil {
				t.Fatal(err)
			}
			r = call(t, w, workbenchEditFile, map[string]any{"path": "hard", "old": "edited", "new": "x"})
			if ToolAvailability(workbenchEditFile) != "" {
				refusedWith(t, r, RefusedNotOffered)
				return
			}
			if !strings.Contains(r.Content, wbLinked) {
				t.Fatal(r)
			}
		})
	}
}

func TestWorkbenchWriteCancellationAtCommit(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	for _, stage := range []string{"chmod", "sync_directory"} {
		t.Run(stage, func(t *testing.T) {
			w, work, _ := testWorkspace(t)
			w.id = newID()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.writeFault = func(s string) error {
				if s == stage {
					cancel()
				}
				return nil
			}
			raw := json.RawMessage(`{"path":"a.txt","content":"committed"}`)
			r, err := w.dispatchWrite(ctx, func() (Result, error) { return w.writeFile(ctx, workbenchWriteFile, raw) })
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(work, "a.txt"))
			if stage == "chmod" {
				if !r.IsError || string(data) != "inside\n" {
					t.Fatalf("%+v %s", r, data)
				}
			} else if r.IsError || string(data) != "committed" {
				t.Fatalf("%+v %s", r, data)
			}
		})
	}
}

func TestWorkbenchRenameErrorJudgedByIdentity(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	for _, effect := range []bool{true, false} {
		t.Run(strconv.FormatBool(effect), func(t *testing.T) {
			w, work, _ := testWorkspace(t)
			w.id = newID()
			w.writeFault = func(stage string) error {
				if stage != "rename" {
					return nil
				}
				entries, _ := os.ReadDir(work)
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".harness-workbench-") {
						if effect {
							if err := w.root.Rename(entry.Name(), "a.txt"); err != nil {
								t.Fatal(err)
							}
						} else {
							if err := w.root.Remove(entry.Name()); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				return errors.New("rename returned an error")
			}
			r := call(t, w, workbenchWriteFile, map[string]any{"path": "a.txt", "content": "new"})
			data, _ := os.ReadFile(filepath.Join(work, "a.txt"))
			if effect {
				if r.IsError || string(data) != "new" {
					t.Fatalf("%+v %s", r, data)
				}
			} else if !strings.Contains(r.Content, wbWriteUnknown) || string(data) != "inside\n" {
				t.Fatalf("%+v %s", r, data)
			}
		})
	}
}

func TestWorkbenchEditExpansionIsBounded(t *testing.T) {
	if contentDisabled(t, workbenchEditFile) {
		return
	}
	w, work, _ := testWorkspace(t)
	w.id = newID()
	writeFile(t, filepath.Join(work, "file"), strings.Repeat("x", 1<<20))
	r := call(t, w, workbenchEditFile, map[string]any{"path": "file", "old": "x", "new": strings.Repeat("y", 1024), "replace_all": true})
	if !strings.Contains(r.Content, wbTooLarge) {
		t.Fatal(r)
	}
	data, _ := os.ReadFile(filepath.Join(work, "file"))
	if len(data) != 1<<20 || data[0] != 'x' {
		t.Fatal("refused expansion changed file")
	}
}
