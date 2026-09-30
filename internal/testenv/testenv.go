// Package testenv lets the test suite run inside a sandbox, such as the one an
// agent runs its commands in. A test that needs something such a sandbox may
// refuse — a Unix domain socket under TMPDIR, a process group of its own —
// calls the matching Require helper first. The helper probes the capability
// once per test binary and skips the test with the probe's refusal when the
// environment denies it with a permission error. Any other probe failure fails
// the test: a real fault is not hidden behind a skip.
//
// Where nothing is refused the tests run as before. CI sets
// AGENT_HARNESS_TEST_NO_SKIP=1, which turns every would-be skip into a
// failure, so an unsandboxed run can never pass by skipping.
package testenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"testing"
)

// NoSkipVariable names the environment variable that turns a refusal into a
// failure instead of a skip.
const NoSkipVariable = "AGENT_HARNESS_TEST_NO_SKIP"

// probe runs check once and keeps its verdict for every later caller.
type probe struct {
	once  sync.Once
	check func() error
	err   error
}

func (p *probe) result() error {
	p.once.Do(func() { p.err = p.check() })
	return p.err
}

// Refusal is a probe's finding that the environment denied an operation.
type Refusal struct {
	Op  string
	Err error
}

func (r *Refusal) Error() string { return r.Op + ": " + r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

var (
	unixSocket    = &probe{check: probeUnixSocket}
	processGroup  = &probe{check: probeProcessGroup}
	processStatus = &probe{check: probeProcessStatus}
	groupPriority = &probe{check: probeGroupPriority}
)

// RequireUnixSocket skips t when the environment refuses to listen on, or
// connect to, a Unix domain socket under the system temporary directory,
// where a restricted session's tool host binds its channel.
func RequireUnixSocket(t testing.TB) {
	t.Helper()
	require(t, "a Unix domain socket under TMPDIR", unixSocket.result())
}

// RequireProcessGroup skips t when the environment refuses to start a
// process in a process group of its own (setpgid).
func RequireProcessGroup(t testing.TB) {
	t.Helper()
	require(t, "a process group of its own (setpgid)", processGroup.result())
}

// RequireProcessStatus skips t when the environment refuses to run ps, which
// tests use to tell a running process from a zombie.
func RequireProcessStatus(t testing.TB) {
	t.Helper()
	require(t, "reading process status with ps", processStatus.result())
}

// RequireGroupPriority skips t when the environment refuses to lower the
// priority of a process group (setpriority), as a background launch does.
func RequireGroupPriority(t testing.TB) {
	t.Helper()
	require(t, "lowering a process group's priority (setpriority)", groupPriority.result())
}

// SkipIfRefused skips t when err is the environment refusing what, fails it
// on any other error, and returns when err is nil. It is for a test's own
// one-off probe, such as a write outside the directories a sandbox allows.
func SkipIfRefused(t testing.TB, what string, err error) {
	t.Helper()
	require(t, what, err)
}

func require(t testing.TB, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if !Refused(err) {
		t.Fatalf("probing %s failed: %v", what, err)
		return
	}
	msg := fmt.Sprintf("environment refuses %s: %v", what, err)
	if os.Getenv(NoSkipVariable) == "1" {
		t.Fatalf("%s (%s=1 forbids skipping)", msg, NoSkipVariable)
		return
	}
	t.Skip(msg)
}

// Refused reports whether err is a permission refusal: the environment, not
// the code under test, denied the operation.
func Refused(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, fs.ErrPermission)
}
