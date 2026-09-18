//go:build windows

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The process starts suspended: assigning a running process would leave a gap
// where children could escape the job. Resume happens only after containment.
// The job is non-inheritable, disallows breakaway, and kills all descendants on
// cancellation or final handle closure. Failure never falls back to a bare child.
type Process struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	job     windows.Handle
	stopped bool
	onStart func(int)
}

// Notify registers a callback invoked once, after the child is running and
// contained, with its process ID. Set it before Run.
func (p *Process) Notify(fn func(int)) { p.onStart = fn }

func New(cmd *exec.Cmd) (*Process, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create harness process job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("contain harness process job: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW, HideWindow: true}
	return &Process{cmd: cmd, job: job}, nil
}

func (p *Process) Run() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return errors.New("harness process cancelled before startup")
	}
	if err := p.cmd.Start(); err != nil {
		p.mu.Unlock()
		return err
	}
	var setupErr error
	if p.stopped {
		setupErr = errors.New("harness process was cancelled before startup")
	} else {
		process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.cmd.Process.Pid))
		if err != nil {
			setupErr = fmt.Errorf("open suspended harness process: %w", err)
		} else {
			setupErr = windows.AssignProcessToJobObject(p.job, process)
			windows.CloseHandle(process)
			if setupErr != nil {
				setupErr = fmt.Errorf("assign harness process job: %w", setupErr)
			} else {
				setupErr = resumePrimaryThread(uint32(p.cmd.Process.Pid))
			}
		}
	}
	if setupErr != nil {
		p.stopLocked()
	}
	pid := p.cmd.Process.Pid
	notify := p.onStart
	p.mu.Unlock()
	if setupErr == nil && notify != nil {
		notify(pid)
	}
	waitErr := p.cmd.Wait()
	if setupErr != nil {
		return setupErr
	}
	return waitErr
}

func (p *Process) Stop() { p.mu.Lock(); defer p.mu.Unlock(); p.stopLocked() }
func (p *Process) stopLocked() {
	p.stopped = true
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
	// Cancellation may arrive between Start and AssignProcessToJobObject. That
	// child is still suspended and cannot have descendants; kill it directly too.
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
func (p *Process) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}

var getProcessIDOfThread = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIdOfThread")

// os/exec closes CreateProcess's initial thread handle. Re-open the sole thread
// of the still-suspended child through the documented Toolhelp APIs, verify its
// owner after opening it, and resume exactly that thread. An unexpected thread
// layout is a containment failure, never permission to resume arbitrary threads.
func resumePrimaryThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("inspect suspended harness thread: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	var threadID uint32
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		if threadID != 0 {
			return errors.New("suspended harness process has an unexpected thread layout")
		}
		threadID = entry.ThreadID
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("inspect harness threads: %w", err)
	}
	if threadID == 0 {
		return errors.New("suspended harness process has no primary thread")
	}
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME|windows.THREAD_QUERY_LIMITED_INFORMATION, false, threadID)
	if err != nil {
		return fmt.Errorf("open suspended harness thread: %w", err)
	}
	defer windows.CloseHandle(thread)
	if err = getProcessIDOfThread.Find(); err != nil {
		return fmt.Errorf("verify harness thread ownership: %w", err)
	}
	owner, _, _ := getProcessIDOfThread.Call(uintptr(thread))
	if uint32(owner) != pid {
		return errors.New("harness thread ownership changed before resume")
	}
	previous, err := windows.ResumeThread(thread)
	if err != nil {
		return fmt.Errorf("resume contained harness process: %w", err)
	}
	if previous != 1 {
		return errors.New("harness primary thread was not in its expected suspended state")
	}
	return nil
}
