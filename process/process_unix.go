//go:build !windows

package process

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// Process contains a CLI and its descendants in a separate process group.
// Construct the command with exec.CommandContext and wire Cancel to Stop.
type Process struct {
	cmd      *exec.Cmd
	mu       sync.Mutex
	started  bool
	finished bool
	stopped  bool
}

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
	err := p.cmd.Start()
	p.started = err == nil
	p.mu.Unlock()
	if err != nil {
		return err
	}
	err = p.cmd.Wait()
	p.mu.Lock()
	p.finished = true
	p.mu.Unlock()
	return err
}

// Stop kills the process group. Calls before Run prevent startup.
func (p *Process) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.started && !p.finished {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Close stops a live group. After Wait has reaped its leader, the numeric
// process-group ID may be reused, so Close must not signal it again.
func (p *Process) Close() { p.Stop() }
