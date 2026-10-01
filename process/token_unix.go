//go:build !windows

package process

import (
	"encoding/hex"
	"errors"
	"os"
	"time"
)

// NewToken returns an unpredictable containment marker for a session.
func NewToken() string { return newLaunchToken() }

// TokenArgument identifies a long-lived supervisor on macOS, whose protected
// platform binaries may hide their environment from process inspection.
func TokenArgument(token string) string { return "--agent-harness-token=" + token }

// TokenEnvironment adds a shared session marker without changing the caller's
// environment. Per-launch containment markers are added independently by Run.
func TokenEnvironment(env []string, token string) []string { return markedEnvironment(env, token) }

// SweepToken stops processes carrying a persisted session marker. As with
// Close, processes that erase their marker cannot be found by this sweep.
func SweepToken(token string, since time.Time) error {
	return sweepToken(token, since, candidates)
}

func sweepToken(token string, since time.Time, inspect func(time.Time) []candidate) error {
	if len(token) != 32 {
		return errors.New("invalid containment token")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return errors.New("invalid containment token")
	}
	// An unavailable scan cannot establish that an old launch is gone.
	if !tokenInspectionAvailable(inspect(time.Time{})) {
		return errors.New("process identity inspection unavailable")
	}
	sweep(token, since)
	if !tokenInspectionAvailable(inspect(time.Time{})) {
		return errors.New("process identity inspection unavailable")
	}
	if TokenPresent(token, since) {
		return errors.New("marked processes have not settled")
	}
	return nil
}

func tokenInspectionAvailable(procs []candidate) bool {
	for _, p := range procs {
		if p.pid == os.Getpid() && p.identity != "" {
			return true
		}
	}
	return false
}

// TokenPresent is positive ownership evidence. False never proves absence;
// the caller may also lack permission to inspect the live process.
func TokenPresent(token string, since time.Time) bool {
	for _, p := range candidates(since.Add(-time.Second)) {
		if p.identity != "" && carriesToken(environment(p.pid), token) && processIdentity(p.pid) == p.identity {
			return true
		}
	}
	return false
}

// GroupHasChildren reports live members other than the group's leader. The
// leader must still have the birth identity captured after Start. Unavailable
// inspection is an error, never evidence that a background job has exited.
func (p *Process) GroupHasChildren() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.finished || p.leaderIdentity == "" {
		return false, errors.New("group identity inspection unavailable")
	}
	pid := p.cmd.Process.Pid
	if processIdentity(pid) != p.leaderIdentity {
		return false, errors.New("group leader identity changed")
	}
	found, children := false, false
	for _, member := range candidates(time.Time{}) {
		if member.pid == pid && member.identity == p.leaderIdentity {
			found = true
		}
		if member.group == pid && member.pid != pid {
			children = true
		}
	}
	if !found {
		return false, errors.New("group inspection unavailable")
	}
	return children, nil
}
