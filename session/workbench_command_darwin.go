package session

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
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

type workbenchToken struct {
	Token string
	Since time.Time
}

func setupWorkbenchCommands(w *workspace, o Options, id string) error {
	if o.Workbench.Commands == nil {
		return nil
	}
	dir := filepath.Join(o.RuntimeHome, "sessions", id)
	tokenPath := filepath.Join(dir, "workbench-token.json")
	if data, err := os.ReadFile(tokenPath); err == nil {
		var old workbenchToken
		if json.Unmarshal(data, &old) != nil || len(old.Token) != 32 || old.Since.IsZero() || old.Since.After(time.Now().Add(time.Second)) {
			return stateError(StateUnusable)
		}
		if _, err := hex.DecodeString(old.Token); err != nil {
			return stateError(StateUnusable)
		}
		if process.SweepToken(old.Token, old.Since) != nil {
			return stateError(StateUnusable)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return stateError(StateUnusable)
	}
	scratch := filepath.Join(dir, "workbench")
	if err := os.RemoveAll(scratch); err != nil {
		return stateError(StateUnusable)
	}
	for _, name := range []string{"home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(scratch, name), 0700); err != nil {
			os.RemoveAll(scratch)
			return stateError(StateUnusable)
		}
	}
	system := o.Workbench.system
	if len(system) == 0 {
		os.RemoveAll(scratch)
		return stateError(StateUnusable)
	}
	token := workbenchToken{process.NewToken(), time.Now()}
	data, _ := json.Marshal(token)
	// Preserve the previous durable marker if a replacement is interrupted.
	tokenTemp := filepath.Join(dir, "workbench-token.tmp")
	_ = os.Remove(tokenTemp)
	f, err := os.OpenFile(tokenTemp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = os.Rename(tokenTemp, tokenPath)
	}
	if err != nil {
		_ = os.Remove(tokenTemp)
	}
	if err == nil {
		r, e := os.OpenRoot(dir)
		if e == nil {
			err = syncWorkbenchDir(r)
			r.Close()
		} else {
			err = e
		}
	}
	if err != nil {
		os.RemoveAll(scratch)
		return stateError(StateUnusable)
	}
	layout := workbenchLayout{Work: o.WorkDir, Home: filepath.Join(scratch, "home"), Tmp: filepath.Join(scratch, "tmp"), System: system, Read: o.Workbench.Commands.Read, Write: o.Workbench.Write, Loopback: o.Workbench.Commands.Loopback}
	env, err := skills.Environment(os.Environ(), append(append([]string{}, o.Workbench.Commands.Env...), "HOME="+layout.Home, "TMPDIR="+layout.Tmp))
	if err != nil {
		os.RemoveAll(scratch)
		return stateError(StateUnusable)
	}
	env = process.TokenEnvironment(env, token.Token)
	var mu sync.Mutex
	type running struct {
		p       *process.Process
		done    chan error
		settled chan struct{}
	}
	var launched []running
	w.commands = &workbenchRunner{timeout: o.Workbench.Commands.Timeout}
	w.commands.close = func() error {
		mu.Lock()
		defer mu.Unlock()
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
	w.commands.run = func(ctx context.Context, command, rel string, timeout time.Duration) (ToolResult, error) {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
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
			return workbenchError(workbenchRunCommand, "command_process_limit", rel), nil
		}
		// Keep the group leader alive after the foreground shell exits. Its
		// argument marker remains visible even when macOS hides platform-binary
		// environments, so recovery can find and sweep ordinary background jobs.
		// Only the supervisor inherits fd 3; the model's shell cannot forge the
		// foreground completion message through that private pipe.
		readStatus, writeStatus, err := os.Pipe()
		if err != nil {
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		defer readStatus.Close()
		keepRead, keepWrite, err := os.Pipe()
		if err != nil {
			writeStatus.Close()
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		defer keepRead.Close()
		args := workbenchSandboxArgs(layout, workbenchSupervisor)
		args = append(args, process.TokenArgument(token.Token), command)
		cmd, p, err := process.Command(context.Background(), "/usr/bin/sandbox-exec", args...)
		if err != nil {
			writeStatus.Close()
			keepWrite.Close()
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
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
		if limit > skills.MaxOutputBytes {
			limit = skills.MaxOutputBytes
		}
		stdout, stderr := &workbenchOutput{limit: limit}, &workbenchOutput{limit: limit}
		outRead, outWrite, e := os.Pipe()
		if e != nil {
			writeStatus.Close()
			keepWrite.Close()
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		errRead, errWrite, e := os.Pipe()
		if e != nil {
			writeStatus.Close()
			keepWrite.Close()
			outRead.Close()
			outWrite.Close()
			return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
		}
		cmd.Stdout = outWrite
		cmd.Stderr = errWrite
		outDone, errDone := make(chan struct{}), make(chan struct{})
		go func() { _, _ = io.Copy(stdout, outRead); close(outDone) }()
		go func() { _, _ = io.Copy(stderr, errRead); close(errDone) }()
		done := make(chan error, 1)
		settled := make(chan struct{})
		var started atomic.Bool
		// Closing the parent's pipe only after Start avoids racing the child's
		// descriptor duplication. Notify also runs only after containment.
		p.Notify(func(int) { started.Store(true); _ = writeStatus.Close(); _ = outWrite.Close(); _ = errWrite.Close() })
		mu.Lock()
		launched = append(launched, running{p, done, settled})
		mu.Unlock()
		go func() {
			err := p.Run()
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
			done <- err
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
				err = <-done
				done <- err
				if started.Load() {
					return workbenchError(workbenchRunCommand, "command_outcome_unknown", rel), errWorkbenchCommandUnknown
				}
				return workbenchError(workbenchRunCommand, "command_start_failed", rel), nil
			}
		case <-runCtx.Done():
			p.Stop()
			// Wait has really settled before this call yields admission. Keep a
			// completed channel value for Close to consume once as well.
			err = <-done
			done <- err
		}
		if ctx.Err() != nil {
			p.Stop()
			<-settled
			return ToolResult{}, ctx.Err()
		}
		// Reap immediately when no background group member remains; otherwise
		// watch the group and reap when the final job exits.
		if children, inspectErr := p.GroupHasChildren(); inspectErr == nil && !children {
			p.Stop()
			<-settled
		} else {
			go reapWorkbenchSupervisor(p, settled)
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
			err = <-done
			done <- err
		}
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		outText, outTruncated := stdout.finish()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			code = -1
		}
		errText, errTruncated := stderr.finish()
		payload, _ := json.Marshal(struct {
			ExitCode  int    `json:"exit_code"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			TimedOut  bool   `json:"timed_out"`
			Truncated bool   `json:"truncated"`
		}{code, outText, errText, errors.Is(runCtx.Err(), context.DeadlineExceeded), outTruncated || errTruncated})
		return ToolResult{Content: string(payload)}, nil
	}
	return nil
}

type workbenchOutput struct {
	mu        sync.Mutex
	frozen    bool
	data      []byte
	limit     int
	truncated bool
}

func (b *workbenchOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.frozen {
		return n, nil
	}
	left := b.limit - len(b.data)
	if left < len(p) {
		p = p[:left]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *workbenchOutput) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.textLocked()
}
func (b *workbenchOutput) textLocked() string {
	s := string(b.data)
	if b.truncated {
		s += "\n[output truncated]"
	}
	return s
}
func (b *workbenchOutput) finish() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frozen = true
	return b.textLocked(), b.truncated
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
