package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestWorkbenchLegacyTemporaryResume(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "removes_only_own", true: "failure_is_unusable"}[fail], func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			o.Workbench.Write = true
			o.complete = (&scriptedModel{}).complete
			s := startAPI(t, o)
			ref := s.Ref()
			closeAPI(t, s)
			store, _, err := openTranscript(o.RuntimeHome, ref)
			if err != nil {
				t.Fatal(err)
			}
			call := completion.ToolCall{ID: "interrupted"}
			call.Function.Name = workbenchWriteFile
			call.Function.Arguments = `{"path":"file","content":"new"}`
			for _, r := range []record{
				{Type: recordTurnStart, Turn: "interrupted"},
				{Type: recordAssistant, Turn: "interrupted", Response: 1, Calls: []completion.ToolCall{call}},
				{Type: recordToolCall, Turn: "interrupted", Response: 1, Call: call.ID, Tool: workbenchWriteFile},
			} {
				if err := store.append(r); err != nil {
					t.Fatal(err)
				}
			}
			store.close()
			own := filepath.Join(o.WorkDir, ".harness-workbench-"+strings.ReplaceAll(ref.ID, "-", "")+"-0123456789abcdef.tmp")
			other := filepath.Join(o.WorkDir, ".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp")
			writeFile(t, own, "old-format partial")
			writeFile(t, other, "other session")
			if fail {
				previous := openWorkspaceFiles
				openWorkspaceFiles = func(config sandbox.Config) (*sandbox.Workspace, error) {
					w, err := sandbox.OpenWorkspace(config)
					if err != nil {
						return nil, err
					}
					*sandboxhook.Access(w).WriteFault = func(stage string) error {
						if stage == "remove_reserved" {
							return errors.New("injected")
						}
						return nil
					}
					return w, nil
				}
				defer func() { openWorkspaceFiles = previous }()
			}
			resumed, err := Resume(context.Background(), o, ref)
			if fail {
				var state *StateError
				if !errors.As(err, &state) || state.Code != StateUnusable {
					if resumed != nil {
						closeAPI(t, resumed)
					}
					t.Fatalf("%v", err)
				}
				if _, err := os.Stat(own); err != nil {
					t.Fatal("uncertain temporary removed", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer closeAPI(t, resumed)
				if _, err := os.Stat(own); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("own temporary survived", err)
				}
				if got := resumed.Recovered().UnknownOutcomes; len(got) != 1 || got[0].ID != call.ID {
					t.Fatalf("%+v", got)
				}
			}
			if data, err := os.ReadFile(other); err != nil || string(data) != "other session" {
				t.Fatal("other session touched", err)
			}
		})
	}
}

// This is the failure branch used by openAPI after preparing commands and
// before admitting a tool. The transcript file is closed to fail its real write.
func TestWorkbenchRecoveryAppendFailureClosesPreparedResources(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume_keeps_state", true: "fresh_removes_state"}[fresh], func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			o.complete = (&scriptedModel{}).complete
			s := startAPI(t, o)
			ref := s.Ref()
			closeAPI(t, s)
			ws, err := openWorkspace(o, ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.close()
			store, records, err := openTranscript(o.RuntimeHome, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			call := completion.ToolCall{ID: "interrupted"}
			call.Function.Name = workbenchWriteFile
			call.Function.Arguments = `{"path":"file","content":"new"}`
			records = append(records, record{Type: recordAssistant, Turn: "interrupted", Response: 1, Calls: []completion.ToolCall{call}})
			var reported error
			a := &apiSession{store: store, workspace: ws, records: records, resultLimit: 4096, failed: func(err error) { reported = err }}
			closes := 0
			ws.commands = &sandbox.Runner{}
			*sandboxhook.RunnerAccess(ws.commands).Close = func() error {
				closes++
				if store.closed {
					t.Error("transcript closed before commands")
				}
				if _, err := sandboxhook.Access(ws.files).Root.Stat("."); err != nil {
					t.Error("workspace closed before commands", err)
				}
				return nil
			}
			if err := store.file.Close(); err != nil {
				t.Fatal(err)
			}
			err = restoreAPIRecovery(a, ws, o, ref, fresh)
			var state *StateError
			if !errors.As(err, &state) || state.Code != StateUnwritable || reported != err {
				t.Fatalf("%v, reported %v", err, reported)
			}
			if closes != 1 || !store.closed {
				t.Fatalf("closes=%d transcript closed=%t", closes, store.closed)
			}
			if _, err := ws.files.Read(context.Background(), json.RawMessage(`{"path":"file"}`)); !errors.Is(err, sandbox.ErrClosed) {
				t.Fatalf("workspace left open: %v", err)
			}
			_, err = os.Stat(sessionDir(o.RuntimeHome, ref.ID))
			if fresh {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fresh failed state retained: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("resume state removed: %v", err)
				}
				reopened, _, err := openTranscript(o.RuntimeHome, ref)
				if err != nil {
					t.Fatal("transcript lock retained", err)
				}
				reopened.close()
			}
		})
	}
}
