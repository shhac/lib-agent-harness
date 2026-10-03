package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/process"
)

// The status pipe belongs to bwrap, never the model's shell. In particular,
// an exec failure is not confused with a shell's ordinary nonzero exit.
func newCommandSandbox(config commandConfig) (*commandSandbox, error) {
	o := config.options
	budget := config.outputBudget
	var commands *commandSandbox
	identity, e := workbenchBinaryFingerprint(config.proof.binary)
	if e != nil || identity != config.proof.identity {
		return nil, stateError(StateUnusable)
	}
	layout, env, token, scratch, err := prepareWorkbenchCommands(o, config.proof, config.stateDir)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	live := make(map[*process.Process]chan struct{})
	closing := false
	commands = &commandSandbox{timeout: o.Timeout}
	commands.close = func() error {
		mu.Lock()
		closing = true
		children := make(map[*process.Process]chan struct{}, len(live))
		for p, settled := range live {
			children[p] = settled
		}
		mu.Unlock()
		for p, settled := range children {
			p.Close()
			<-settled
		}
		if process.SweepToken(token.Token, token.Since) != nil || os.RemoveAll(scratch) != nil {
			return &CommandError{Code: "command_cleanup_unknown"}
		}
		return nil
	}
	commands.execute = func(ctx context.Context, command, rel string, timeout time.Duration, onStart func()) (CommandResult, error) {
		current, e := workbenchBinaryFingerprint(config.proof.binary)
		if e != nil || current != identity {
			return CommandResult{}, commandError("command_start_failed")
		}
		runCtx, cancel := commandContext(ctx, timeout)
		defer cancel()
		args, e := bwrapArgs(layout)
		if e != nil {
			return CommandResult{}, commandError("command_start_failed")
		}
		r, status, e := os.Pipe()
		if e != nil {
			return CommandResult{}, commandError("command_start_failed")
		}
		defer r.Close()
		defer status.Close()
		args = append(args, "--json-status-fd", "3", "--chdir", filepath.Join(o.WorkDir, filepath.FromSlash(rel)), "--", "/bin/sh", "-c", command)
		binary := config.proof.binary
		if o.Background {
			binary, args = linuxBackgroundLaunch(binary, args)
		}
		cmd, p, e := process.Command(runCtx, binary, args...)
		if e != nil {
			return CommandResult{}, commandError("command_start_failed")
		}
		cmd.Env = env
		cmd.ExtraFiles = []*os.File{status}
		cmd.WaitDelay = 2 * time.Second
		limit := (budget - 1024) / 12
		if config.standalone {
			limit = skills.MaxOutputBytes
		}
		if limit > skills.MaxOutputBytes {
			limit = skills.MaxOutputBytes
		}
		if limit < 0 {
			limit = 0
		}
		stdout, stderr := &workbenchOutput{limit: limit}, &workbenchOutput{limit: limit}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		p.Notify(func(int) { status.Close() })
		settled := make(chan struct{})
		mu.Lock()
		if closing {
			mu.Unlock()
			p.Close()
			return CommandResult{}, context.Canceled
		}
		if len(live) >= 64 {
			mu.Unlock()
			p.Close()
			return CommandResult{}, commandError(CommandProcessLimit)
		}
		live[p] = settled
		mu.Unlock()
		completed := make(chan error, 1)
		go func() { e := p.Run(); status.Close(); completed <- e }()
		started, code, known := readBwrapStatus(r, onStart)
		<-completed
		p.Close()
		mu.Lock()
		delete(live, p)
		close(settled)
		mu.Unlock()
		if ctx.Err() != nil {
			return CommandResult{}, ctx.Err()
		}
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		if timedOut {
			code = -1
		} else if !started {
			return CommandResult{}, commandError("command_start_failed")
		} else if !known {
			return CommandResult{}, commandError("command_outcome_unknown")
		}
		out, ot := stdout.finish()
		errout, et := stderr.finish()
		return CommandResult{code, out, errout, timedOut, ot || et}, nil
	}
	return commands, nil
}
