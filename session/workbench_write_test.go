package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/sandbox"
)

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
	*sandboxhook.Access(s.api.workspace.files).WriteFault = func(at string) error {
		if at == stage {
			os.Exit(0)
		}
		return nil
	}
	runAPITurnToEnd(t, s, "write")
	t.Fatal("crash hook was not reached")
}

func TestWorkbenchWriteCrashResumeNeverReruns(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
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
			*sandboxhook.Access(resumed.api.workspace.files).WriteFault = func(string) error { t.Error("recovered call was rerun"); return errors.New("rerun") }
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
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	o := workbenchOptions(t, nopHandler())
	w, err := openWorkspace(o, newID())
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	work := o.WorkDir
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
	if sandbox.ToolAvailability("read_file") != "" {
		if len(defs) != 2 || defs[0].Name != workbenchListFiles || defs[1].Name != workbenchWriteFile {
			t.Fatal(defs)
		}
		return
	}
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
	if sandbox.ToolAvailability("read_file") != "" {
		if len(defs) != 3 || defs[2].Name != workbenchRunCommand {
			t.Fatal(defs)
		}
		return
	}
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

func TestWorkbenchWriteUnknownIsRecorded(t *testing.T) {
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Write = true
	model := &scriptedModel{steps: [][]scriptedCall{{{"write", "write_file", `{"path":"file","content":"new"}`}}, {{"finish", "finish", `{}`}}}}
	o.complete = model.complete
	s := startAPI(t, o)
	defer closeAPI(t, s)
	*sandboxhook.Access(s.api.workspace.files).WriteFault = func(stage string) error {
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
	testenv.RequireAtomicWrite(t) // The outer command boundary refuses reserved atomic temporaries.
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
	want := "new"
	if sandbox.ToolAvailability("edit_file") != "" {
		want = "old"
	}
	if string(data) != want {
		t.Fatalf("%s", data)
	}
}
