package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestCommandSandboxHostedResultContract(t *testing.T) {
	s := &workbenchHost{commands: &sandbox.Runner{}}
	sandboxhook.RunnerAccess(s.commands).SetExecute(func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{ExitCode: 3, Stdout: "out", Stderr: "err"}, nil
	})
	r, err := s.run(context.Background(), "", ".", time.Second)
	if err != nil || r.Content != `{"exit_code":3,"stdout":"out","stderr":"err","timed_out":false,"truncated":false}` {
		t.Fatalf("%+v %v", r, err)
	}
	sandboxhook.RunnerAccess(s.commands).SetExecute(func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{}, &sandbox.CommandError{Code: CommandOutcomeUnknown}
	})
	r, err = s.run(context.Background(), "", ".", time.Second)
	if !errors.Is(err, errWorkbenchCommandUnknown) || !r.IsError || r.Content != "run_command error: command_outcome_unknown: \".\"" {
		t.Fatalf("%+v %v", r, err)
	}
	sandboxhook.RunnerAccess(s.commands).SetExecute(func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{}, &sandbox.CommandError{Code: CommandSandboxClosed}
	})
	r, err = s.run(context.Background(), "", ".", time.Second)
	if !errors.Is(err, context.Canceled) || r.IsError || r.Content != "" {
		t.Fatalf("hosted closure contract changed: %+v %v", r, err)
	}
}
