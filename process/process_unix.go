//go:build !windows

package process

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Process contains a CLI and its descendants in a separate process group, and
// marks their environment so Stop and Close can also find the descendants that
// left it, such as a server an agent backgrounded. Build it with Command,
// which wires the command's cancellation to Stop.
type Process struct {
	cmd        *exec.Cmd
	mu         sync.Mutex
	started    bool
	finished   bool
	stopped    bool
	onStart    func(int)
	token      string
	launched   time.Time
	background bool
}

// Notify registers a callback invoked once, after the child is running and
// contained, with its process ID. A caller that has to record what it launched
// before it can safely recover from a crash needs that identity, and needs it
// only when containment actually took effect. Set it before Run.
func (p *Process) Notify(fn func(int)) { p.onStart = fn }

// Background lowers the whole contained tree to background priority once it
// starts: its process group is niced, and on macOS it also enters the
// kernel's background band, which throttles its CPU and I/O. Descendants
// inherit both, including ones started later in a process group of their own.
// Set it before Run.
func (p *Process) Background() { p.background = true }

// New prepares containment before any child starts.
func New(cmd *exec.Cmd) (*Process, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &Process{cmd: cmd}, nil
}

// Run starts the contained command and waits for its output to drain.
func (p *Process) Run() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return errors.New("harness process cancelled before startup")
	}
	p.token, p.launched = newLaunchToken(), time.Now()
	p.cmd.Env = markedEnvironment(p.cmd.Env, p.token)
	err := p.cmd.Start()
	p.started = err == nil
	pid := 0
	if err == nil {
		pid = p.cmd.Process.Pid
	}
	if err == nil && p.background {
		if lowerErr := lowerPriority(pid); lowerErr != nil {
			// Asked for background work and not given it: stop rather than
			// compete with the foreground at normal priority.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			p.mu.Unlock()
			_ = p.cmd.Wait()
			return lowerErr
		}
	}
	notify := p.onStart
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if notify != nil {
		notify(pid)
	}
	err = p.cmd.Wait()
	p.mu.Lock()
	p.finished = true
	p.mu.Unlock()
	return err
}

// Stop kills the process group and every marked descendant. Calls before Run
// prevent startup.
func (p *Process) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	if p.started && !p.finished {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	started := p.started
	p.mu.Unlock()
	if started {
		sweep(p.token, p.launched)
	}
}

// Close stops a live group and kills whatever marked descendants remain, also
// after the CLI itself exited: what its agent started must not outlive the
// handle. After Wait has reaped the leader, the numeric process-group ID may be
// reused, so Close must not signal that group again; the sweep matches each
// process by its own marker instead.
func (p *Process) Close() {
	p.Stop()
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if started {
		sweep(p.token, p.launched)
	}
}
