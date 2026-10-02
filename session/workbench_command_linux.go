package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/process"
)

// The status pipe belongs to bwrap, never the model's shell. In particular,
// an exec failure is not confused with a shell's ordinary nonzero exit.
func setupWorkbenchCommands(w *workspace, o Options, id string) error {
	if o.Workbench.Commands == nil {
		return nil
	}
	identity, e := workbenchBinaryFingerprint(o.Workbench.commandBinary)
	if e != nil || identity != o.Workbench.commandIdentity {
		return stateError(StateUnusable)
	}
	layout, env, token, scratch, err := prepareWorkbenchCommands(o, id)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var live *process.Process
	var done chan struct{}
	closing := false
	w.commands = &workbenchRunner{timeout: o.Workbench.Commands.Timeout}
	w.commands.close = func() error {
		mu.Lock()
		closing = true
		p, settled := live, done
		mu.Unlock()
		if p != nil {
			p.Close()
			<-settled
		}
		if process.SweepToken(token.Token, token.Since) != nil || os.RemoveAll(scratch) != nil {
			return &TurnError{Engine: harness.OpenAICompatible, Code: "command_cleanup_unknown"}
		}
		return nil
	}
	w.commands.run = func(ctx context.Context, command, rel string, timeout time.Duration) (ToolResult, error) {
		current, e := workbenchBinaryFingerprint(o.Workbench.commandBinary)
		if e != nil || current != identity {
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		args, e := bwrapArgs(layout)
		if e != nil {
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		r, status, e := os.Pipe()
		if e != nil {
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		defer r.Close()
		defer status.Close()
		args = append(args, "--json-status-fd", "3", "--chdir", filepath.Join(o.WorkDir, filepath.FromSlash(rel)), "--", "/bin/sh", "-c", command)
		binary := o.Workbench.commandBinary
		if o.Background {
			binary, args = linuxBackgroundLaunch(binary, args)
		}
		cmd, p, e := process.Command(runCtx, binary, args...)
		if e != nil {
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		cmd.Env = env
		cmd.ExtraFiles = []*os.File{status}
		cmd.WaitDelay = 2 * time.Second
		limit := (w.budget - 1024) / 12
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
			return ToolResult{}, context.Canceled
		}
		live, done = p, settled
		mu.Unlock()
		completed := make(chan error, 1)
		go func() { e := p.Run(); status.Close(); completed <- e }()
		started, code, known := readBwrapStatus(r)
		<-completed
		p.Close()
		mu.Lock()
		live = nil
		close(settled)
		mu.Unlock()
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		if timedOut {
			code = -1
		} else if !started {
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		} else if !known {
			return workbenchError(workbenchRunCommand, "command_outcome_unknown", rel), errWorkbenchCommandUnknown
		}
		out, ot := stdout.finish()
		errout, et := stderr.finish()
		payload, _ := json.Marshal(struct {
			ExitCode  int    `json:"exit_code"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			TimedOut  bool   `json:"timed_out"`
			Truncated bool   `json:"truncated"`
		}{code, out, errout, timedOut, ot || et})
		return ToolResult{Content: string(payload)}, nil
	}
	return nil
}
