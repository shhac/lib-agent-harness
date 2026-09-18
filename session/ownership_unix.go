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
// session. Containment needs a process group that can be signalled and probed,
// and an advisory lock a dying process releases. A platform without both is
// refused rather than run without containment.
func restrictedPlatform() bool { return true }

// openNoFollow opens a file without traversing a final symbolic link, so a link
// planted where a credential belongs cannot redirect a read or a write.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func ownerOnly(info fs.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("tool host directory must be readable only by its owner")
	}
	return nil
}

// groupAlive asks the operating system whether any process remains in a group.
// A group with no members is positive evidence that the subtree launched into
// it is gone. A group with members is not evidence that they are ours: group
// identifiers are reused, which is why identity is established separately.
//
// A process that has exited but not been reaped still counts as a member. That
// is the conservative direction — it reports presence, never absence — and in
// the situation this exists for, the process that could have reaped it has
// already died, so its children belong to init and are reaped promptly.
func groupAlive(group int) (bool, error) {
	if group <= 1 {
		return false, errors.New("recorded process group is not a contained harness")
	}
	err := syscall.Kill(-group, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		// Something exists that this user may not signal. It is alive, and it is
		// certainly not the harness this process launched.
		return true, nil
	default:
		return false, errors.New("process group liveness could not be established")
	}
}

// holdBridgeLock takes the exclusive lock a bridge holds for its whole life and
// records which launch it belongs to. The launch identifier lets recovery tell
// this bridge apart from an unrelated process that inherited the same group
// number, so a stored integer is never the sole basis for signalling.
func holdBridgeLock(path, launch string) (*os.File, error) {
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
	record, _ := json.Marshal(owner{Group: group, PID: os.Getpid(), Parent: os.Getppid(), Launch: launch, HeldAt: time.Now().UTC()})
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

// readBridgeLock reports the owner recorded by a bridge that currently holds
// the lock, or nil when no bridge holds it.
//
// A nil result means no bridge is running. It does NOT mean the harness is
// gone: a harness can outlive its tool server, restart it, or be in inference
// with none running. Callers must not read it as absence.
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
	if n == 0 || json.Unmarshal(trimNull(raw[:n]), &held) != nil {
		// Held by something whose record cannot be read. That is unknown, and an
		// empty owner carries no launch identity, so it can never authorize a kill.
		return &owner{}, nil
	}
	return &held, nil
}

// holdLease takes the assignment lease for a session's private directory. It is
// acquired before anything is launched and held for the session's lifetime, so
// two processes cannot drive the same assignment even before a bridge exists.
func holdLease(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("session lease is unavailable")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, ErrLeaseHeld
	}
	return f, nil
}

// terminateGroup signals a contained harness. Callers must have established
// that the group is theirs; this only refuses the cases that are never right.
//
// A group signal can be refused outright — the kernel reports that when it
// could not deliver to any member — so the identified leader is signalled as
// well. Both are attempted, and neither is treated as proof: the caller
// confirms from the group afterwards.
func terminateGroup(group, leader int) error {
	if group <= 1 {
		return errors.New("recorded process group is not a contained harness")
	}
	if own, err := syscall.Getpgid(0); err == nil && own == group {
		return errors.New("recorded process group is this process's own group")
	}
	groupErr := signalled(syscall.Kill(-group, syscall.SIGKILL))
	// The leader is the process whose identity was established, so signalling it
	// directly needs no further justification. Descendants that outlive it are
	// caught by the group signal, or reported as unresolved.
	var leaderErr error
	if leader > 1 && leader != os.Getpid() {
		leaderErr = signalled(syscall.Kill(leader, syscall.SIGKILL))
	}
	if groupErr == nil || leaderErr == nil {
		return nil
	}
	return errors.New("recorded harness could not be signalled")
}

// signalled treats an absent target as success: the point of the signal is that
// the process is gone, and it already is.
func signalled(err error) error {
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func trimNull(raw []byte) []byte {
	for i, b := range raw {
		if b == 0 {
			return raw[:i]
		}
	}
	return raw
}
