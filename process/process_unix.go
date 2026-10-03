//go:build !windows

package process

import (
	"errors"
	"os"
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
	cmd            *exec.Cmd
	mu             sync.Mutex
	started        bool
	finished       bool
	stopped        bool
	onStart        func(int)
	token          string
	launched       time.Time
	leaderIdentity string
	background     bool
}

// Notify registers a callback invoked once, after the child is running and
// contained, with its process ID. A caller that has to record what it launched
// before it can safely recover from a crash needs that identity, and needs it
// only when containment actually took effect. Set it before Run.
func (p *Process) Notify(fn func(int)) { p.onStart = fn }

// Background lowers the whole contained tree to background priority once it
// starts: its process group is niced, which descendants inherit, including
// ones started later in a process group of their own. Set it before Run.
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
		p.leaderIdentity = processIdentity(pid)
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
	// A leader killed, perhaps by its own child, left its group with no
	// orderly end, so what is left of it goes now; one that exited leaves
	// what it started running until the handle closes.
	if killed(p.cmd.ProcessState) {
		p.reapGroup()
	}
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
		identity := processIdentity(p.cmd.Process.Pid)
		if p.leaderIdentity == "" || identity == "" || identity == p.leaderIdentity {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		} else {
			// Only a positively different birth identity proves PID reuse.
			// An exited leader or unavailable inspection does not mean its
			// still-live group disappeared (children may hold output pipes).
			_ = p.cmd.Process.Kill()
		}
	}
	started := p.started
	p.mu.Unlock()
	if started {
		sweep(p.token, p.launched)
	}
}

// Close stops a live group and kills whatever marked descendants remain, also
// after the CLI itself exited: what its agent started must not outlive the
// handle. It reaps what is left in the leader's group, checking each member
// by birth identity rather than trusting a numeric process-group ID that may
// have been reused, and then what carries the launch's marker.
func (p *Process) Close() {
	p.Stop()
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if started {
		p.reapGroup()
		sweep(p.token, p.launched)
	}
}

// killed reports a process that ended on a signal.
func killed(state *os.ProcessState) bool {
	if state == nil {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

// reapGroup kills what is left in the leader's process group when the handle
// closes, even members whose environment the system hides, so cannot be
// swept by their marker; until then they live on, as a server an agent
// backgrounded must. A leader whose PID was positively reused is never
// signalled.
func (p *Process) reapGroup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd.Process == nil || p.leaderIdentity == "" {
		return
	}
	pid := p.cmd.Process.Pid
	identity := processIdentity(pid)
	if identity != "" && identity != p.leaderIdentity {
		return
	}
	for _, member := range candidates(p.launched.Add(-time.Second)) {
		if member.group != pid || member.pid == pid {
			continue
		}
		// Revalidate both birth identity and group immediately before
		// signalling its group. A live member anchors the pgid, so it
		// cannot be recycled between validation and the group signal.
		signalOwned(member.pid, member.identity, processIdentity, func(child int) {
			leader := processIdentity(pid)
			if leader != "" && leader != p.leaderIdentity {
				return
			}
			if group, err := syscall.Getpgid(child); err == nil && group == pid {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		})
	}
}
