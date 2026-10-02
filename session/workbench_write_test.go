package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
)

func TestWorkbenchAtomicWriteAndEdit(t *testing.T) {
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
		r := call(t, w, tc.tool, tc.args)
		if r.IsError {
			t.Fatal(r.Content)
		}
		data, e := os.ReadFile(filepath.Join(work, "new", "dir", "file"))
		if e != nil || string(data) != tc.want {
			t.Fatalf("%q %v", data, e)
		}
	}
	r := call(t, w, workbenchEditFile, map[string]any{"path": "new/dir/file", "old": "two", "new": "x"})
	if !strings.Contains(r.Content, "match_not_unique") {
		t.Fatal(r)
	}
	r = call(t, w, workbenchEditFile, map[string]any{"path": "new/dir/file", "old": "missing", "new": "x"})
	if !strings.Contains(r.Content, "match_not_found") {
		t.Fatal(r)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(work, "new", "dir", "file"))
		if info.Mode().Perm() != 0644 {
			t.Fatal(info.Mode())
		}
	}
}

func TestWorkbenchWriteRefusals(t *testing.T) {
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
	if !r.IsError || !strings.Contains(r.Content, wbReserved) {
		t.Fatal(r)
	}
}

func TestWorkbenchBackgroundWithoutCommandsRefused(t *testing.T) {
	for _, wb := range []*Workbench{nil, {}, {Write: true}} {
		o := workbenchOptions(t, nopHandler())
		o.Background, o.Workbench = true, wb
		if wb == nil {
			o.WorkDir = ""
		}
		_, err := Start(context.Background(), o)
		var refused *UnsupportedError
		if !errors.As(err, &refused) || refused.Operation != "background" {
			t.Fatalf("%v", err)
		}
		entries, err := os.ReadDir(o.RuntimeHome)
		if err != nil || len(entries) != 0 {
			t.Fatalf("refusal touched runtime: %v %v", entries, err)
		}
	}
}

// This child exits in the file operation, before the host can record a result
// or Close can clean the temporary. It uses only a scripted model.
func TestWorkbenchWriteCrashHelper(t *testing.T) {
	stage := os.Getenv("WORKBENCH_CRASH_STAGE")
	if stage == "" {
		return
	}
	o := workbenchOptions(t, nopHandler())
	o.RuntimeHome = os.Getenv("WORKBENCH_CRASH_RUNTIME")
	o.WorkDir = os.Getenv("WORKBENCH_CRASH_WORK")
	o.Workbench = &Workbench{Write: true}
	o.complete = (&scriptedModel{steps: [][]scriptedCall{{{"write", "write_file", `{"path":"file","content":"new"}`}}}}).complete
	s := startAPI(t, o)
	data, err := json.Marshal(s.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("WORKBENCH_CRASH_REF"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s.api.workspace.writeFault = func(at string) error {
		if at == stage {
			os.Exit(0)
		}
		return nil
	}
	runAPITurnToEnd(t, s, "write")
	t.Fatal("crash hook was not reached")
}

func TestWorkbenchWriteCrashResumeNeverReruns(t *testing.T) {
	for _, stage := range []string{"chmod", "sync_directory"} {
		t.Run(stage, func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			o.Workbench = &Workbench{Write: true}
			writeFile(t, filepath.Join(o.WorkDir, "file"), "old")
			refPath := filepath.Join(t.TempDir(), "ref.json")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkbenchWriteCrashHelper$")
			cmd.Env = append(os.Environ(), "WORKBENCH_CRASH_STAGE="+stage, "WORKBENCH_CRASH_RUNTIME="+o.RuntimeHome, "WORKBENCH_CRASH_WORK="+o.WorkDir, "WORKBENCH_CRASH_REF="+refPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("crash child: %v: %s", err, out)
			}
			data, err := os.ReadFile(refPath)
			if err != nil {
				t.Fatal(err)
			}
			var ref Ref
			if err := json.Unmarshal(data, &ref); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(o.WorkDir, "file"))
			if err != nil || (string(before) != "old" && string(before) != "new") {
				t.Fatalf("torn target: %q %v", before, err)
			}
			want := "old"
			if stage == "sync_directory" {
				want = "new"
			}
			if string(before) != want {
				t.Fatalf("stage %s: %q", stage, before)
			}
			ownPrefix := ".harness-workbench-" + strings.ReplaceAll(ref.ID, "-", "") + "-"
			entries, err := os.ReadDir(o.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ownPrefix) {
					found = true
				}
			}
			if stage == "chmod" && !found {
				t.Fatal("crash did not leave its temporary")
			}
			other := filepath.Join(o.WorkDir, ".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp")
			writeFile(t, other, "another session")
			model := &scriptedModel{steps: [][]scriptedCall{{{"finish", "finish", `{}`}}}}
			o.complete = model.complete
			resumed, err := Resume(context.Background(), o, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAPI(t, resumed)
			if recovered := resumed.Recovered(); len(recovered.UnknownOutcomes) != 1 || recovered.UnknownOutcomes[0] != (RecoveredCall{ID: "write", Tool: workbenchWriteFile}) {
				t.Fatalf("%+v", recovered)
			}
			// Any rerun of the dangling write would fail this hook.
			resumed.api.workspace.writeFault = func(string) error { t.Error("recovered call was rerun"); return errors.New("rerun") }
			done := runAPITurnToEnd(t, resumed, "inspect the recovery")
			if done.err != nil || done.result.Status != "completed" {
				t.Fatalf("%+v %v", done.result, done.err)
			}
			after, err := os.ReadFile(filepath.Join(o.WorkDir, "file"))
			if err != nil || string(after) != string(before) {
				t.Fatalf("recovery changed target: %q %v", after, err)
			}
			entries, err = os.ReadDir(o.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ownPrefix) {
					t.Fatal("own temporary survived Resume")
				}
			}
			if data, err := os.ReadFile(other); err != nil || string(data) != "another session" {
				t.Fatal("other session's temporary changed")
			}
			records, _ := readTranscript(t, o.RuntimeHome, ref.ID)
			unknown := 0
			for _, r := range records {
				if r.Type == recordToolResult && r.Call == "write" {
					if r.Outcome != outcomeUnknown || !r.IsError {
						t.Fatal(r)
					}
					unknown++
				}
			}
			if unknown != 1 {
				t.Fatalf("recovered results: %d", unknown)
			}
		})
	}
}

func TestWorkbenchNewReadOnlyModeCreatesParents(t *testing.T) {
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
	for _, mode := range []os.FileMode{0755, 0640} {
		t.Run(mode.String(), func(t *testing.T) {
			w, work, _ := testWorkspace(t)
			w.id = newID()
			file := filepath.Join(work, "a.txt")
			if os.Chmod(file, mode) != nil {
				t.Fatal("chmod")
			}
			r := call(t, w, workbenchEditFile, map[string]any{"path": "a.txt", "old": "inside", "new": "edited"})
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
			r = call(t, w, workbenchEditFile, map[string]any{"path": "hard", "old": "missing", "new": "x"})
			if !strings.Contains(r.Content, wbLinked) {
				t.Fatal(r)
			}
		})
	}
}

func TestWorkbenchWriteCancellationAtCommit(t *testing.T) {
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
			r, err := w.dispatchWrite(ctx, func() (ToolResult, error) { return w.writeFile(ctx, workbenchWriteFile, raw) })
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

func TestWorkbenchWriteDigestAndResume(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true}
	o.complete = (&scriptedModel{}).complete
	s := startAPI(t, o)
	ref := s.Ref()
	closeAPI(t, s)
	changed := o
	changed.Workbench = &Workbench{Write: true, NewFileMode: 0644}
	before, _ := os.ReadFile(filepath.Join(o.RuntimeHome, "sessions", ref.ID, "transcript.jsonl"))
	if _, err := Resume(context.Background(), changed, ref); !errors.Is(err, ErrIncompatibleResume) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(o.RuntimeHome, "sessions", ref.ID, "transcript.jsonl"))
	if string(before) != string(after) {
		t.Fatal("transcript changed")
	}
	o.Workbench = &Workbench{Write: true, NewFileMode: 0600}
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	closeAPI(t, resumed)
	o.Workbench = &Workbench{}
	a := apiReference(o, "id")
	o.Workbench.NewFileMode = 0644
	if apiReference(o, "id") != a {
		t.Fatal("read-only mode changed digest")
	}
}

func TestWorkbenchCleanupOnlyOwnTemporary(t *testing.T) {
	w, work, _ := testWorkspace(t)
	w.id = newID()
	own := ".harness-workbench-" + strings.ReplaceAll(w.id, "-", "") + "-0123456789abcdef.tmp"
	other := ".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(work, own), "partial")
	writeFile(t, filepath.Join(work, other), "other")
	call := completion.ToolCall{ID: "call"}
	call.Function.Name = workbenchWriteFile
	call.Function.Arguments = `{"path":"a.txt","content":"new"}`
	if err := w.cleanupWrites([]record{{Type: recordAssistant, Response: 1, Calls: []completion.ToolCall{call}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, own)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own temporary survived")
	}
	if _, err := os.Stat(filepath.Join(work, other)); err != nil {
		t.Fatal("other session touched")
	}
}

func TestWorkbenchCommandsUnsupportedPlatforms(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("darwin proves commands")
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{}
	_, err := Start(context.Background(), o)
	var refused *UnsupportedError
	if !errors.As(err, &refused) || refused.Capability != harness.Support(harness.OpenAICompatible, harness.Session, harness.Sandbox) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(o.RuntimeHome)
	if len(entries) != 0 {
		t.Fatal("state created before refusal")
	}
}

func TestWorkbenchWriteDefinitions(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Write = true
	defs := workbenchDefinitions(o)
	if len(defs) != 5 || defs[3].Name != workbenchWriteFile || defs[4].Name != workbenchEditFile {
		t.Fatal(defs)
	}
	encoded, _ := json.Marshal(completionTools(defs))
	if len(encoded) != 2508 {
		t.Fatalf("write definitions encoded size: %d", len(encoded))
	}
}

func TestWorkbenchCommandDefinitions(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Write = true
	o.Workbench.Commands = &Commands{}
	defs := workbenchDefinitions(o)
	if len(defs) != 6 || defs[5].Name != workbenchRunCommand {
		t.Fatal(defs)
	}
	encoded, err := json.Marshal(completionTools(defs))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 2893 {
		t.Fatalf("command definitions encoded size: %d", len(encoded))
	}
}

func TestWorkbenchEditExpansionIsBounded(t *testing.T) {
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

func TestWorkbenchWriteUnknownIsRecorded(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Write = true
	model := &scriptedModel{steps: [][]scriptedCall{{{"write", "write_file", `{"path":"file","content":"new"}`}}, {{"finish", "finish", `{}`}}}}
	o.complete = model.complete
	s := startAPI(t, o)
	defer closeAPI(t, s)
	s.api.workspace.writeFault = func(stage string) error {
		if stage == "sync_directory" {
			return errors.New("injected")
		}
		return nil
	}
	done := runAPITurnToEnd(t, s, "write")
	if done.err != nil {
		t.Fatal(done.err)
	}
	found := false
	for _, ev := range done.events {
		if ev.Kind == "tool_completed" && ev.Tool == workbenchWriteFile {
			found = ev.ErrorFacts != nil && ev.ErrorFacts.Family == harness.FailureTurn && ev.ErrorFacts.Code == wbWriteUnknown && !ev.ErrorFacts.Retryable
		}
	}
	if !found {
		t.Fatal("unknown failure facts absent")
	}
	s.api.mu.Lock()
	defer s.api.mu.Unlock()
	found = false
	for _, r := range s.api.records {
		if r.Type == recordToolResult && r.Call == "write" {
			found = r.Outcome == outcomeUnknown
		}
	}
	if !found {
		t.Fatal("unknown outcome absent")
	}
	data, _ := os.ReadFile(filepath.Join(o.WorkDir, "file"))
	if string(data) != "new" {
		t.Fatal("committed write rolled back")
	}
}

func TestWorkbenchWriteLoop(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Write = true
	model := &scriptedModel{steps: [][]scriptedCall{
		{{"list", "list_files", `{}`}},
		{{"write", "write_file", `{"path":"file","content":"old"}`}},
		{{"edit", "edit_file", `{"path":"file","old":"old","new":"new"}`}},
		{{"read", "read_file", `{"path":"file"}`}},
		{{"finish", "finish", `{}`}},
	}}
	o.complete = model.complete
	s := startAPI(t, o)
	defer closeAPI(t, s)
	done := runAPITurnToEnd(t, s, "write and edit")
	if done.err != nil || done.result.Status != "completed" {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	data, _ := os.ReadFile(filepath.Join(o.WorkDir, "file"))
	if string(data) != "new" {
		t.Fatalf("%s", data)
	}
}
