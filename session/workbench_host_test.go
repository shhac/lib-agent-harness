package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestWorkbenchWorkspaceConfiguration(t *testing.T) {
	previous := openWorkspaceFiles
	defer func() { openWorkspaceFiles = previous }()
	var configs []sandbox.Config
	openWorkspaceFiles = func(config sandbox.Config) (*sandbox.Workspace, error) {
		configs = append(configs, config)
		return sandbox.OpenWorkspace(config)
	}
	o := workbenchOptions(t, nopHandler())
	o.complete = (&scriptedModel{}).complete
	o.Workbench.NewFileMode = 0644
	o.Restriction.Tools.MaxResultBytes = 4096
	s := startAPI(t, o)
	ref := s.Ref()
	closeAPI(t, s)
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	closeAPI(t, resumed)
	if len(configs) != 2 {
		t.Fatalf("opened %d workspaces", len(configs))
	}
	for _, config := range configs {
		if config.SessionID != ref.ID || config.OnFailure == nil || config.NewFileMode != 0644 || config.Budget != 4096 {
			t.Fatalf("wrong workspace configuration: %+v", config)
		}
	}
}

// Every entry point reconstructs availability without altering the persisted
// configuration digest or replaying completed/refused historical calls.
func TestWorkbenchDisabledContentSessionLifecycle(t *testing.T) {
	if sandbox.ToolAvailability("read_file") == "" {
		return
	}
	for _, config := range []struct {
		name            string
		write, commands bool
	}{
		{"default", false, false}, {"write", true, false}, {"commands", true, true}, {"commands_read_only", false, true},
	} {
		t.Run(config.name, func(t *testing.T) {
			for _, entry := range []string{"Start", "Open", "Resume"} {
				t.Run(entry, func(t *testing.T) {
					calls := 0
					o := workbenchOptions(t, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
						calls++
						return ToolResult{Content: "finished"}, nil
					}))
					o.Workbench.Write = config.write
					if config.write {
						testenv.RequireAtomicWrite(t)
					}
					workRoot, e := filepath.EvalSymlinks(o.WorkDir)
					if e != nil {
						t.Fatal(e)
					}
					runtimeHome, e := filepath.EvalSymlinks(o.RuntimeHome)
					if e != nil {
						t.Fatal(e)
					}
					commandCalls, proofs, runners := 0, 0, 0
					if config.commands {
						o.Workbench.Commands = &Commands{}
						previousProof, previousRunner := sandboxbridge.ProveWorkbench, newWorkbenchRunner
						defer func() { sandboxbridge.ProveWorkbench = previousProof; newWorkbenchRunner = previousRunner }()
						sandboxbridge.ProveWorkbench = func(_ context.Context, options any) (any, error) {
							proofs++
							n := options.(sandbox.Options)
							if n.WorkDir != workRoot || n.Write != config.write {
								t.Errorf("proof options: %+v", n)
							}
							return sandbox.Proof{}, nil
						}
						newWorkbenchRunner = func(n sandbox.Options, _ sandbox.Proof, state string, budget int) (*sandbox.Runner, error) {
							runners++
							if n.WorkDir != workRoot || !strings.HasPrefix(state, runtimeHome) || budget < minWorkbenchResult {
								t.Errorf("runner configuration: %+v %s %d", n, state, budget)
							}
							r := &sandbox.Runner{}
							sandboxhook.RunnerAccess(r).SetExecute(func(_ context.Context, command, rel string, _ time.Duration, _ func()) (sandbox.CommandResult, error) {
								commandCalls++
								if command != "synthetic check" || rel != "." {
									t.Errorf("command: %q dir: %q", command, rel)
								}
								return sandbox.CommandResult{ExitCode: 0, Stdout: "synthetic command success"}, nil
							})
							*sandboxhook.RunnerAccess(r).Close = func() error { return nil }
							return r, nil
						}
					}
					model := &scriptedModel{steps: [][]scriptedCall{
						{{"read", "read_file", `{"path":"a.txt"}`}, {"search", "search_files", `{"pattern":"x"}`}, {"edit", "edit_file", `{"path":"a.txt","old":"x","new":"y"}`}},
						{{"list", "list_files", `{}`}},
						{{"finish", "finish", `{}`}},
					}}
					if config.write {
						model.steps = append(model.steps[:2], []scriptedCall{{"write", "write_file", `{"path":"file","content":"surviving write"}`}}, model.steps[2])
					}
					if config.commands {
						last := len(model.steps) - 1
						model.steps = append(model.steps[:last], []scriptedCall{{"command", "run_command", `{"command":"synthetic check"}`}}, model.steps[last])
					}
					o.complete = model.complete
					var s *Session
					var err error
					switch entry {
					case "Start":
						s, err = Start(context.Background(), o)
					case "Open":
						s, _, err = Open(context.Background(), o, nil)
					case "Resume":
						prior, e := Start(context.Background(), o)
						if e != nil {
							t.Fatal(e)
						}
						ref := prior.Ref()
						closeAPI(t, prior)
						s, err = Resume(context.Background(), o, ref)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer closeAPI(t, s)
					caps := s.Capabilities()
					wantCount := 3
					if config.write {
						wantCount = 5
					}
					if config.commands {
						wantCount++
					}
					if len(caps.WorkbenchTools) != wantCount {
						t.Fatal(caps.WorkbenchTools)
					}
					var capabilityNames []string
					for _, tool := range caps.WorkbenchTools {
						capabilityNames = append(capabilityNames, tool.Name)
						disabled := sandbox.ToolAvailability(tool.Name) != ""
						if disabled && (tool.Capability.Availability != harness.Unsupported || tool.Capability.Reason != sandbox.FileToolsDisabledReason) {
							t.Fatal(tool)
						}
						if !disabled && tool.Capability.Availability != harness.Composed {
							t.Fatal(tool)
						}
					}
					expectedCapabilities := "read_file,list_files,search_files"
					if config.write {
						expectedCapabilities += ",write_file,edit_file"
					}
					if config.commands {
						expectedCapabilities += ",run_command"
					}
					if strings.Join(capabilityNames, ",") != expectedCapabilities {
						t.Fatalf("capability names: %v", capabilityNames)
					}
					caps.WorkbenchTools[0].Name = "mutated"
					if s.Capabilities().WorkbenchTools[0].Name == "mutated" {
						t.Fatal("capabilities aliased")
					}
					if strings.Count(s.api.system, sandbox.FileToolsDisabledReason) != 1 || s.options.Instructions.Text != o.Instructions.Text {
						t.Fatal("absence notice changed configuration")
					}
					done := runAPITurnToEnd(t, s, "work")
					if done.err != nil || !s.ToolsClosed() || calls != 1 {
						t.Fatalf("%v closed=%v calls=%d", done.err, s.ToolsClosed(), calls)
					}
					if config.write {
						data, e := os.ReadFile(filepath.Join(o.WorkDir, "file"))
						if e != nil || string(data) != "surviving write" {
							t.Fatalf("write result %q: %v", data, e)
						}
					}
					wantedPreparations := 1
					if entry == "Resume" {
						wantedPreparations = 2
					}
					if config.commands && (commandCalls != 1 || proofs != wantedPreparations || runners != wantedPreparations) {
						t.Fatalf("command=%d proof=%d runner=%d", commandCalls, proofs, runners)
					}
					names := strings.Join(offeredNames(model.tools[0]), ",")
					want := "finish,list_files"
					if config.write {
						want += ",write_file"
					}
					if config.commands {
						want += ",run_command"
					}
					if names != want {
						t.Fatalf("advertised %s", names)
					}
					refused := 0
					for _, record := range s.api.records {
						if record.Type == recordToolResult && (record.Call == "read" || record.Call == "search" || record.Call == "edit") {
							if record.Outcome != outcomeRefused || !strings.Contains(record.Text, "not available") {
								t.Fatal(record)
							}
							refused++
						}
					}
					permittedResults := 0
					for _, record := range s.api.records {
						if record.Type == recordToolResult && (record.Call == "list" || record.Call == "write" || record.Call == "command") {
							permittedResults++
							if record.Call == "command" && !strings.Contains(record.Text, "synthetic command success") {
								t.Fatal(record)
							}
							if record.IsError || record.Outcome != outcomeRan {
								t.Fatalf("surviving operation refused: %+v", record)
							}
						}
					}
					wantedResults := 1
					if config.write {
						wantedResults++
					}
					if config.commands {
						wantedResults++
					}
					if permittedResults != wantedResults {
						t.Fatalf("permitted results %d, want %d", permittedResults, wantedResults)
					}
					if refused != 3 {
						t.Fatal("missing refusal results", refused)
					}
					ref := s.Ref()
					closeAPI(t, s)
					before, _ := readTranscript(t, o.RuntimeHome, ref.ID)
					resumed, e := Resume(context.Background(), o, ref)
					if e != nil {
						t.Fatal(e)
					}
					if resumed.Ref().ConfigHash != ref.ConfigHash || strings.Count(resumed.api.system, sandbox.FileToolsDisabledReason) != 1 {
						t.Fatal("resume compatibility or notice changed")
					}
					after, _ := readTranscript(t, o.RuntimeHome, ref.ID)
					if len(before) != len(after) || calls != 1 || (config.commands && commandCalls != 1) {
						t.Fatal("historical calls replayed")
					}
					if config.commands && (proofs != wantedPreparations+1 || runners != wantedPreparations+1) {
						t.Fatal("resume did not reconstruct commands")
					}
					closeAPI(t, resumed)
				})
			}
		})
	}
}

func TestWorkbenchHistoricalContentRecovery(t *testing.T) {
	if sandbox.ToolAvailability("read_file") == "" {
		return
	}
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
	calls := []completion.ToolCall{}
	for _, name := range []string{"read_file", "search_files", "edit_file", "read_file"} {
		c := completion.ToolCall{ID: name}
		if len(calls) == 3 {
			c.ID = "unstarted"
		}
		c.Function.Name = name
		c.Function.Arguments = `{}`
		calls = append(calls, c)
	}
	records := []record{
		{Type: recordTurnStart, Turn: "old"},
		{Type: recordAssistant, Turn: "old", Response: 1, Calls: calls},
		{Type: recordToolCall, Turn: "old", Response: 1, Call: "read_file", Tool: "read_file"},
		{Type: recordToolResult, Turn: "old", Response: 1, Call: "read_file", Tool: "read_file", Outcome: outcomeRan, Text: "historical content"},
		{Type: recordToolResult, Turn: "old", Response: 1, Call: "search_files", Tool: "search_files", Outcome: outcomeRefused, Text: "historical refusal", IsError: true},
		{Type: recordToolCall, Turn: "old", Response: 1, Call: "edit_file", Tool: "edit_file"},
	}
	for _, r := range records {
		if err := store.append(r); err != nil {
			store.close()
			t.Fatal(err)
		}
	}
	store.close()
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI(t, resumed)
	outcomes := map[string]string{}
	for _, r := range resumed.api.records {
		if r.Type == recordToolResult {
			outcomes[r.Call] = r.Outcome
			if r.Call == "read_file" && r.Text != "historical content" {
				t.Fatal("historical result rewritten")
			}
		}
	}
	for id, want := range map[string]string{"read_file": outcomeRan, "search_files": outcomeRefused, "edit_file": outcomeUnknown, "unstarted": outcomeNotRun} {
		if outcomes[id] != want {
			t.Fatalf("%s: %s", id, outcomes[id])
		}
	}
	if got := resumed.Recovered().UnknownOutcomes; len(got) != 1 || got[0].Tool != "edit_file" {
		t.Fatal(got)
	}
}
