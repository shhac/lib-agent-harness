package session

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/process"
)

func newCommandSandbox(config commandConfig) (*commandSandbox, error) {
	o, id := config.options, config.id
	w := &workspace{budget: config.outputBudget}
	if o.Workbench.Commands == nil {
		return nil, nil
	}
	layout, env, token, scratch, err := prepareWorkbenchCommands(o, id)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	type running struct {
		p       *process.Process
		done    chan struct{}
		settled chan struct{}
	}
	var launched []running
	closing := false
	w.commands = &commandSandbox{timeout: o.Workbench.Commands.Timeout}
	w.commands.close = func() error {
		mu.Lock()
		defer mu.Unlock()
		closing = true
		for _, child := range launched {
			child.p.Close()
			<-child.done
		}
		if process.SweepToken(token.Token, token.Since) != nil {
			return &TurnError{Engine: harness.OpenAICompatible, Code: "command_cleanup_unknown"}
		}
		if os.RemoveAll(scratch) != nil {
			return &TurnError{Engine: harness.OpenAICompatible, Code: "command_cleanup_unknown"}
		}
		return nil
	}
	w.commands.execute = func(ctx context.Context, command, rel string, timeout time.Duration, onStart func()) (CommandResult, error) {
		runCtx, cancel := commandContext(ctx, timeout)
		defer cancel()
		mu.Lock()
		live := launched[:0]
		for _, child := range launched {
			select {
			case <-child.settled:
				child.p.Close()
				<-child.done
			default:
				live = append(live, child)
			}
		}
		clear(launched[len(live):])
		launched = live
		full := len(launched) >= 64
		mu.Unlock()
		if full {
			return CommandResult{}, commandError("command_process_limit")
		}
		// Keep the group leader alive after the foreground shell exits. Its
		// argument marker remains visible even when macOS hides platform-binary
		// environments, so recovery can find and sweep ordinary background jobs.
		// Only the supervisor inherits fd 3; the model's shell cannot forge the
		// foreground completion message through that private pipe.
		readStatus, writeStatus, err := os.Pipe()
		if err != nil {
			return CommandResult{}, commandError("command_start_failed")
		}
		defer readStatus.Close()
		keepRead, keepWrite, err := os.Pipe()
		if err != nil {
			writeStatus.Close()
			return CommandResult{}, commandError("command_start_failed")
		}
		defer keepRead.Close()
		args := workbenchSandboxArgs(layout, workbenchSupervisor)
		args = append(args, process.TokenArgument(token.Token), command)
		cmd, p, err := process.Command(context.Background(), "/usr/bin/sandbox-exec", args...)
		if err != nil {
			writeStatus.Close()
			keepWrite.Close()
			return CommandResult{}, commandError("command_start_failed")
		}
		// The supervisor also owns the writer (fd 5), so launcher death does
		// not release its marker while background group members still live.
		cmd.ExtraFiles = []*os.File{writeStatus, keepRead, keepWrite}
		cmd.Dir = filepath.Join(o.WorkDir, filepath.FromSlash(rel))
		cmd.Env = env
		cmd.WaitDelay = 2 * time.Second
		if o.Background {
			p.Background()
		}
		limit := (w.budget - 1024) / 12
		if o.Workbench.standaloneCommands {
			limit = skills.MaxOutputBytes
		}
		if limit > skills.MaxOutputBytes {
			limit = skills.MaxOutputBytes
		}
		stdout, stderr := &workbenchOutput{limit: limit}, &workbenchOutput{limit: limit}
		outRead, outWrite, e := os.Pipe()
		if e != nil {
			writeStatus.Close()
			keepWrite.Close()
			return CommandResult{}, commandError("command_start_failed")
		}
		errRead, errWrite, e := os.Pipe()
		if e != nil {
			writeStatus.Close()
			keepWrite.Close()
			outRead.Close()
			outWrite.Close()
			return CommandResult{}, commandError("command_start_failed")
		}
		cmd.Stdout = outWrite
		cmd.Stderr = errWrite
		outDone, errDone := make(chan struct{}), make(chan struct{})
		go func() { _, _ = io.Copy(stdout, outRead); close(outDone) }()
		go func() { _, _ = io.Copy(stderr, errRead); close(errDone) }()
		done := make(chan struct{})
		settled := make(chan struct{})
		var started atomic.Bool
		// Closing the parent's pipe only after Start avoids racing the child's
		// descriptor duplication. Notify also runs only after containment.
		p.Notify(func(int) {
			started.Store(true)
			notifyCommandLaunch(onStart)
			_ = writeStatus.Close()
			_ = outWrite.Close()
			_ = errWrite.Close()
		})
		mu.Lock()
		if closing || len(launched) >= 64 {
			code := CommandProcessLimit
			if closing {
				code = CommandSandboxClosed
			}
			mu.Unlock()
			p.Close()
			writeStatus.Close()
			outWrite.Close()
			errWrite.Close()
			keepWrite.Close()
			outRead.Close()
			errRead.Close()
			<-outDone
			<-errDone
			return CommandResult{}, commandError(code)
		}
		launched = append(launched, running{p, done, settled})
		mu.Unlock()
		go func() {
			_ = p.Run()
			_ = writeStatus.Close()
			_ = outWrite.Close()
			_ = errWrite.Close()
			keepWrite.Close()
			// Run has killed remaining members before returning. Closing the
			// readers now settles copiers even if a detached job kept a pipe.
			drained := make(chan struct{})
			go func() { <-outDone; <-errDone; close(drained) }()
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
			}
			_ = outRead.Close()
			_ = errRead.Close()
			<-drained
			close(done)
			close(settled)
		}()
		status := make(chan string, 1)
		go func() { line, _ := bufio.NewReader(readStatus).ReadString('\n'); status <- line }()
		code := -1
		select {
		case line := <-status:
			if value, e := strconv.Atoi(strings.TrimSpace(line)); e == nil {
				code = value
			} else {
				p.Stop()
				<-done
				if ctx.Err() != nil {
					return CommandResult{}, ctx.Err()
				}
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
					break
				}
				if started.Load() {
					return CommandResult{}, commandError("command_outcome_unknown")
				}
				return CommandResult{}, commandError("command_start_failed")
			}
		case <-runCtx.Done():
			p.Stop()
			// The broadcast completion channel lets both the call and Close wait
			// for real settlement without consuming one another's observation.
			<-done
		}
		if ctx.Err() != nil {
			p.Stop()
			<-settled
			return CommandResult{}, ctx.Err()
		}
		// Reap immediately when no background group member remains; otherwise
		// watch the group and reap when the final job exits.
		if children, inspectErr := p.GroupHasChildren(); inspectErr == nil && !children {
			p.Stop()
			<-settled
		} else {
			go reapWorkbenchSupervisor(p, settled)
		}
		if timeout == 0 {
			select {
			case <-settled:
			case <-ctx.Done():
				p.Stop()
				<-settled
			}
		}
		// Allow the bounded output copiers to drain data already in the pipes;
		// background output is then discarded for the rest of the session.
		drain := time.NewTimer(2 * time.Second)
		defer drain.Stop()
		select {
		case <-outDone:
			select {
			case <-errDone:
			case <-drain.C:
			case <-runCtx.Done():
			}
		case <-drain.C:
		case <-runCtx.Done():
		}
		// finish freezes capture; the copiers keep consuming and discarding
		// late background output until the process group has settled.
		if runCtx.Err() != nil {
			p.Stop()
			<-done
		}
		if ctx.Err() != nil {
			return CommandResult{}, ctx.Err()
		}
		outText, outTruncated := stdout.finish()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			code = -1
		}
		errText, errTruncated := stderr.finish()
		return CommandResult{code, outText, errText, errors.Is(runCtx.Err(), context.DeadlineExceeded), outTruncated || errTruncated}, nil
	}
	return w.commands, nil
}

const workbenchSupervisor = `trap '' PIPE
/bin/sh -c "$1" 3>&- 4>&- 5>&-
status=$?
printf '%s\n' "$status" >&3
exec 3>&-
exec >/dev/null 2>&1
IFS= read -r _ <&4
`

// Inspection failures preserve ownership until Close. The launch limit bounds
// resources even when inspection remains unavailable.
func reapWorkbenchSupervisor(p *process.Process, settled <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-settled:
			return
		case <-ticker.C:
			if children, err := p.GroupHasChildren(); err == nil && !children {
				p.Stop()
				return
			}
		}
	}
}
