package session

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

func TestWorkbenchMountCheckRefusesBeforeTranscript(t *testing.T) {
	resumeOptions := workbenchOptions(t, nopHandler())
	resumeOptions.complete = (&scriptedModel{}).complete
	existing := startAPI(t, resumeOptions)
	ref := existing.Ref()
	closeAPI(t, existing)
	before := snapshot(t, resumeOptions.RuntimeHome)
	previous := *sandboxhook.MountID
	*sandboxhook.MountID = func(*os.File) (wsfile.Mount, error) { return wsfile.Mount{}, wsfile.ErrMountUnavailable }
	defer func() { *sandboxhook.MountID = previous }()
	o := workbenchOptions(t, nopHandler())
	for _, open := range []func() error{
		func() error { _, e := Start(context.Background(), o); return e },
		func() error { _, _, e := Open(context.Background(), o, nil); return e },
	} {
		if code := workbenchRefusal(t, open()); code != RefusedWorkbenchMountCheck {
			t.Fatal(code)
		}
	}
	if _, err := Resume(context.Background(), resumeOptions, ref); workbenchRefusal(t, err) != RefusedWorkbenchMountCheck {
		t.Fatal(err)
	}
	if !sameFiles(before, snapshot(t, resumeOptions.RuntimeHome)) {
		t.Fatal("refused resume changed the transcript")
	}
	if entries, _ := os.ReadDir(o.RuntimeHome); len(entries) != 0 {
		t.Fatal(entries)
	}
}

func TestWorkspaceWorkerStuckFailsSessionAndReleasesLock(t *testing.T) {
	for _, control := range []string{"cancel", "interrupt", "steer", "close"} {
		t.Run(control, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			controlDone := make(chan error, 1)
			o := workbenchOptions(t, nopHandler())
			o.complete = (&scriptedModel{steps: [][]scriptedCall{{{"read", "read_file", `{"path":"file"}`}}}}).complete
			writeFile(t, o.WorkDir+"/file", "inside")
			s, err := Start(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			w := sandboxhook.Access(s.api.workspace.files)
			*w.Grace = 20 * time.Millisecond
			entered, release := make(chan struct{}), make(chan struct{})
			steps := 0
			*w.Step = func() {
				steps++
				if steps == 2 {
					close(entered)
					<-release
				}
			}
			defer func() {
				close(release)
				select {
				case <-w.Stopped:
				case <-time.After(time.Second):
					t.Error("stuck worker did not exit after release")
				}
				if w.Handles.Load() != 0 {
					t.Error("worker leaked handles:", w.Handles.Load())
				}
				awaitGoroutineBaseline(t, baseline)
			}()
			turn, err := s.StartTurn(context.Background(), Input{Text: "read"})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if w.Handles.Load() != 1 {
				t.Fatal("stalled read did not retain its file handle")
			}
			switch control {
			case "cancel":
				s.CancelTools()
			case "interrupt":
				go func() { controlDone <- s.Interrupt(context.Background(), turn.ID()) }()
			case "steer":
				go func() {
					_, err := s.Steer(context.Background(), turn.ID(), Input{Text: "again"}, SteerOptions{})
					controlDone <- err
				}()
			case "close":
				s.Close()
			}
			select {
			case <-s.api.released:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown stuck")
			}
			if h := s.Health(); h.State != Failed || h.Reason != WorkspaceIOStuck {
				t.Fatalf("%+v", h)
			}
			if _, err = s.Release(context.Background()); !workspaceStuck(err) {
				t.Fatal(err)
			}
			if _, err = s.StartTurn(context.Background(), Input{Text: "again"}); err == nil {
				t.Fatal("admitted after failure")
			}
			_, err = turn.Wait(context.Background())
			if control == "close" {
				if !errors.Is(err, ErrClosed) {
					t.Fatal(err)
				}
			} else if !workspaceStuck(err) {
				t.Fatal(err)
			}
			if control == "interrupt" || control == "steer" {
				if err := <-controlDone; !workspaceStuck(err) {
					t.Fatal(err)
				}
			}
			records := s.api.records
			found := false
			failedTurn := false
			for _, r := range records {
				if r.Type == recordTurnEnd {
					failedTurn = r.Status == "failed" && r.Code == WorkspaceIOStuck
				}
				if r.Type == recordToolResult && r.Call == "read" {
					found = r.Outcome == outcomeUnknown && r.Text == unknownOutcomeText
				}
			}
			if !found || !failedTurn {
				t.Fatalf("unknown result missing: %+v", records)
			}
			resumed, _, err := Open(context.Background(), o, ptrRef(s.Ref()))
			if err != nil {
				t.Fatal(err)
			}
			closeAPI(t, resumed)
		})
	}
}

func ptrRef(r Ref) *Ref { return &r }

func TestWorkspaceFailurePreservesFirstCause(t *testing.T) {
	stuck := &TurnError{Code: WorkspaceIOStuck}
	for _, first := range []error{nil, ErrClosed, ErrProtocol} {
		s := &Session{closed: true, failure: first}
		s.failWorkspace(stuck)
		if first == ErrProtocol {
			if s.failure != first {
				t.Fatal("first failure was overwritten")
			}
		} else if s.failure != stuck {
			t.Fatal("late workspace failure was lost")
		}
	}
	released := make(chan struct{})
	close(released)
	w := &workbenchHost{}
	w.stuck.Store(stuck)
	s := &Session{closed: true, failure: ErrProtocol, api: &apiSession{workspace: w, released: released}}
	if _, err := s.Release(context.Background()); err != stuck {
		t.Fatal("Release lost abandoned I/O:", err)
	}
}

func TestWorkbenchInterruptRetainsFailedTurnReason(t *testing.T) {
	turn := &Turn{id: "turn", done: make(chan struct{}), events: make(chan Event)}
	failure := &TurnError{Engine: harness.OpenAICompatible, Code: WorkspaceIOStuck}
	s := &Session{active: turn, options: Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}}}
	s.api = &apiSession{cancelTurn: func() {
		s.mu.Lock()
		s.failure = failure
		s.mu.Unlock()
		// A terminal status without its error still has the session's typed cause.
		turn.finish("failed", nil)
	}}
	if err := s.interrupt(context.Background(), turn.ID()); err != failure {
		t.Fatalf("lost workspace failure: %v", err)
	}
}

func awaitGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline: %d, want at most %d", runtime.NumGoroutine(), baseline)
}
