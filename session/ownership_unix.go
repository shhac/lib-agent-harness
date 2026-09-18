//go:build !windows

package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// restrictedPlatform reports whether this build can contain a restricted
// session. Containment needs an advisory lock that a dying process releases and
// a process group that can be signalled; a platform without both is refused
// rather than run without containment.
func restrictedPlatform() bool { return true }

func ownerOnly(info fs.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("tool host directory must be readable only by its owner")
	}
	return nil
}

// holdBridgeLock takes the exclusive lock a bridge holds for its whole life and
// records the process group that holds it. A caller that finds this lock free
// knows no bridge — and therefore no harness subtree it belongs to — survives.
func holdBridgeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("bridge lock is unavailable")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("another bridge already holds this session's tool channel")
	}
	group, err := syscall.Getpgid(0)
	if err != nil {
		_ = f.Close()
		return nil, errors.New("bridge process group is unavailable")
	}
	record, _ := json.Marshal(owner{Group: group, PID: os.Getpid(), Parent: os.Getppid(), HeldAt: time.Now().UTC()})
	if err = f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, errors.New("bridge lock could not be recorded")
	}
	if _, err = f.WriteAt(record, 0); err != nil {
		_ = f.Close()
		return nil, errors.New("bridge lock could not be recorded")
	}
	return f, nil
}

// readBridgeLock reports the recorded owner when the lock is held, and a nil
// owner when it is free. A free lock is positive evidence of absence; anything
// else is treated as presence, because the cost of assuming otherwise is a
// second worker running against the same assignment.
func readBridgeLock(path string) (*owner, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("bridge lock could not be inspected")
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return nil, nil
	}
	raw := make([]byte, 512)
	n, _ := f.ReadAt(raw, 0)
	var held owner
	if n == 0 || json.Unmarshal(trimNull(raw[:n]), &held) != nil || held.Group <= 1 {
		return &owner{}, nil
	}
	return &held, nil
}

// terminateGroup signals a whole process group. It refuses groups that are not
// plausibly a contained harness, and never signals the caller's own group.
func terminateGroup(group int) error {
	if group <= 1 {
		return errors.New("recorded process group is not a contained harness")
	}
	if own, err := syscall.Getpgid(0); err == nil && own == group {
		return errors.New("recorded process group is this process's own group")
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return errors.New("recorded process group could not be signalled")
	}
	return nil
}

func trimNull(raw []byte) []byte {
	for i, b := range raw {
		if b == 0 {
			return raw[:i]
		}
	}
	return raw
}
