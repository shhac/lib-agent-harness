// Package testenv lets the test suite run inside a sandbox, such as the one an
// agent runs its commands in. A test that needs something such a sandbox may
// refuse — a Unix domain socket under TMPDIR, a process group of its own —
// calls the matching Require helper first. The helper probes the capability
// once per test binary and skips the test with the probe's refusal when the
// environment denies it with a permission error or a limit the library itself
// refuses. Any other probe failure fails the test: a real fault is not hidden behind a skip.
//
// Where nothing is refused the tests run as before. CI sets
// AGENT_HARNESS_TEST_NO_SKIP=1, which turns every would-be skip into a
// failure, so an unsandboxed run can never pass by skipping.
package testenv

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	atomicWrite   = &probe{check: probeAtomicWrite}
	nestedSandbox = &probe{check: probeNestedSandbox}
	loopback      = &probe{check: probeLoopback}
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
	msg := strings.Join(strings.Fields(fmt.Sprintf("environment refuses %s: %v", what, err)), " ")
	if os.Getenv(NoSkipVariable) == "1" {
		t.Fatalf("%s (%s=1 forbids skipping)", msg, NoSkipVariable)
		return
	}
	t.Skip(msg)
}

// Refused reports a permission refusal or the explicit channel-path limit.
// Bare EINVAL and other unexpected probe errors remain faults.
func Refused(err error) bool {
	return errors.Is(err, ErrSocketPathTooLong) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, fs.ErrPermission)
}

// ErrSocketPathTooLong identifies the channel-directory limit in session/privatefs.go.
var ErrSocketPathTooLong = errors.New("socket path exceeds the library's 90-byte channel-directory limit")

// RequireNestedSandbox probes a trivial OS sandbox independently of generated profiles.
func RequireNestedSandbox(t testing.TB) {
	t.Helper()
	require(t, "a nested OS sandbox", nestedSandbox.result())
}

// RequireLoopback probes listening and connecting on IPv4 localhost.
func RequireLoopback(t testing.TB) {
	t.Helper()
	require(t, "a loopback listener", loopback.result())
}

// Listen checks an actual test listener; unexpected errors still fail loudly.
func Listen(t testing.TB, network, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen(network, addr)
	SkipIfRefused(t, "a "+network+" listener", err)
	return l
}

func probeLoopback() error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return &Refusal{Op: "listen", Err: err}
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		return &Refusal{Op: "connect", Err: err}
	}
	return c.Close()
}

// RequireAtomicWrite probes reserved file-tool temporaries. An outer command
// sandbox deliberately protects these names, even below its writable TMPDIR.
func RequireAtomicWrite(t testing.TB) {
	t.Helper()
	require(t, "creating an atomic workbench temporary", atomicWrite.result())
}

func probeAtomicWrite() error {
	dir, err := os.MkdirTemp("", "ah-write-")
	if err != nil {
		return &Refusal{Op: "mkdir", Err: err}
	}
	defer os.RemoveAll(dir)
	name := filepath.Join(dir, ".harness-workbench-00000000000000000000000000000000-0123456789abcdef.tmp")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return &Refusal{Op: "create", Err: err}
	}
	defer f.Close()
	if _, err := f.Write([]byte("probe")); err != nil {
		return &Refusal{Op: "write", Err: err}
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	var b [5]byte
	if _, err := f.Read(b[:]); err != nil {
		return &Refusal{Op: "read", Err: err}
	}
	return nil
}
