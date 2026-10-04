package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/sandbox"
)

// No socket or model is needed: exercise real admission, CancelTools and
// settlement around the command handler, with a synthetic transport.
func TestHostedCommandFailureDiagnosticsAtSettlement(t *testing.T) {
	for _, budget := range []int{minWorkbenchResult, maxWorkbenchResult} {
		for _, cause := range []string{"unknown", "cancel", "deadline", "generic", "closed", "start"} {
			t.Run(cause+"/"+strconv.Itoa(budget), func(t *testing.T) {
				note := "[harness PATH: dropped \"/unreadable/node\" (outside-read-set); ]\n"
				stderr := note + strings.Repeat("\x00\"\\", budget)
				runner := &sandbox.Runner{}
				w := &workbenchHost{commands: runner, budget: budget}
				entered, release := make(chan struct{}), make(chan struct{})
				sandboxhook.RunnerAccess(runner).SetExecute(func(ctx context.Context, _ string, _ string, _ time.Duration, _ func()) (CommandResult, error) {
					close(entered)
					if cause == "cancel" {
						<-ctx.Done()
					}
					<-release
					result := CommandResult{ExitCode: -1, Stderr: stderr, Truncated: true}
					switch cause {
					case "unknown":
						return result, &sandbox.CommandError{Code: CommandOutcomeUnknown}
					case "closed":
						return result, &sandbox.CommandError{Code: CommandSandboxClosed}
					case "start":
						return result, &sandbox.CommandError{Code: CommandStartFailed}
					case "cancel":
						return result, ctx.Err()
					case "deadline":
						return result, context.DeadlineExceeded
					default:
						return result, errors.New(strings.Repeat("\"", 10000))
					}
				})
				idle := make(chan struct{})
				close(idle)
				h := &toolHost{
					cfg: ToolHost{MaxResultBytes: budget, Handler: ToolHandlerFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
						return w.run(ctx, "command", ".", time.Second)
					})},
					tools:   map[string]ToolDefinition{workbenchRunCommand: {Name: workbenchRunCommand}},
					pending: map[string]*hostedCall{}, gate: make(chan struct{}, 1), done: make(chan struct{}), settled: idle,
				}
				ready, refusal := h.prepareCall("call", workbenchRunCommand, json.RawMessage("{}"))
				if refusal != nil {
					t.Fatal(refusal)
				}
				recorded := make(chan toolOutcome, 1)
				finished := make(chan struct{})
				go func() { h.execute(ready, "turn", func(out toolOutcome) { recorded <- out }); close(finished) }()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("runner not admitted")
				}
				s := &Session{tools: h}
				if cause == "cancel" {
					s.CancelTools()
					if err := h.readyForWork(); !errors.Is(err, ErrToolsUnsettled) {
						t.Fatal("cancellation authorized unfinished work", err)
					}
				}
				if err := h.readyForWork(); !errors.Is(err, ErrToolsUnsettled) {
					t.Fatal("unfinished command authorized work", err)
				}
				close(release)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := s.AwaitToolsSettled(ctx); err != nil {
					t.Fatal(err)
				}
				<-finished
				out := <-recorded
				if !out.ran || !out.isError || len(out.text) > budget {
					t.Fatalf("bad outcome %+v", out)
				}
				wantUnknown := cause == "unknown" || cause == "cancel" || cause == "deadline" || cause == "closed"
				if out.unknown != wantUnknown {
					t.Fatalf("unknown-effect semantics changed: %+v", out)
				}
				if cause == "unknown" && out.code != CommandOutcomeUnknown {
					t.Fatal(out.code)
				}
				var payload struct {
					Stderr    string
					Truncated bool
					Error     string
				}
				if err := json.Unmarshal([]byte(out.text), &payload); err != nil {
					t.Fatalf("failure payload invalid: %v", err)
				}
				if !strings.HasPrefix(payload.Stderr, note) || strings.Count(payload.Stderr, "[harness PATH:") != 1 || !payload.Truncated || payload.Error == "" {
					t.Fatalf("lost diagnostics: %+v", payload)
				}
				if wantUnknown && cause != "unknown" && !strings.Contains(payload.Error, "effect is unknown") {
					t.Fatal(payload.Error)
				}
			})
		}
	}
}

func TestCommandFailureWithoutCapturedDiagnosticsKeepsContract(t *testing.T) {
	w := &workbenchHost{budget: minWorkbenchResult}
	legacy := workbenchError(workbenchRunCommand, CommandOutcomeUnknown, ".")
	got := w.commandFailureResult(CommandResult{}, legacy)
	if got != legacy || w.commandInterruptedResult(CommandResult{}).Content != "" {
		t.Fatal(got)
	}
}

func TestHostedCommandCaptureContracts(t *testing.T) {
	for _, budget := range []int{minWorkbenchResult, maxWorkbenchResult} {
		for _, cause := range []string{"success", "timeout", "generic", "cancel", "unknown", "start", "closed"} {
			t.Run(cause+"/"+strconv.Itoa(budget), func(t *testing.T) {
				runner := &sandbox.Runner{}
				note := "[harness PATH: dropped unreadable]\n"
				sandboxhook.RunnerAccess(runner).SetExecute(func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
					switch cause {
					case "generic":
						return CommandResult{}, errors.New("legacy generic")
					case "cancel":
						return CommandResult{}, context.Canceled
					case "unknown":
						return CommandResult{}, &sandbox.CommandError{Code: CommandOutcomeUnknown}
					case "start":
						return CommandResult{}, &sandbox.CommandError{Code: CommandStartFailed}
					case "closed":
						return CommandResult{}, &sandbox.CommandError{Code: CommandSandboxClosed}
					}
					return CommandResult{Stderr: note, TimedOut: cause == "timeout"}, nil
				})
				w := &workbenchHost{commands: runner, budget: budget}
				idle := make(chan struct{})
				close(idle)
				h := &toolHost{cfg: ToolHost{MaxResultBytes: budget, Handler: ToolHandlerFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
					return w.run(ctx, "command", ".", time.Second)
				})}, tools: map[string]ToolDefinition{workbenchRunCommand: {Name: workbenchRunCommand}}, pending: map[string]*hostedCall{}, gate: make(chan struct{}, 1), done: make(chan struct{}), settled: idle}
				ready, refusal := h.prepareCall("call", workbenchRunCommand, json.RawMessage("{}"))
				if refusal != nil {
					t.Fatal(refusal)
				}
				out := h.execute(ready, "turn", nil)
				if len(out.text) > budget || !out.ran {
					t.Fatal(out)
				}
				switch cause {
				case "success", "timeout":
					var result CommandResult
					if json.Unmarshal([]byte(out.text), &result) != nil || strings.Count(result.Stderr, "[harness PATH:") != 1 || out.isError || result.TimedOut != (cause == "timeout") {
						t.Fatal(out)
					}
				case "generic":
					if out.text != "legacy generic" || out.unknown {
						t.Fatal(out)
					}
				case "cancel", "closed":
					if out.text != cancelledToolText || !out.unknown {
						t.Fatal(out)
					}
				default:
					if out.text != workbenchError(workbenchRunCommand, map[string]string{"unknown": CommandOutcomeUnknown, "start": CommandStartFailed}[cause], ".").Content {
						t.Fatal(out)
					}
				}
			})
		}
	}
}
