//go:build darwin || linux

package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// escapee runs a shell that backgrounds a helper, which moves itself into a
// process group of its own, and exits, as Claude Code does with a background
// command: the helper is reparented and outside the contained group. The
// helper is this test binary, not sleep: macOS hides the environment of its
// own platform binaries.
func escapee(t *testing.T, ctx context.Context, linger string) (*Process, func() int) {
	t.Helper()
	cmd, p, err := Command(ctx, "/bin/sh", "-c", `"$0" -test.run='^TestHelper$' -- detached >/dev/null 2>&1 & echo $!; `+linger, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "HARNESS_PROCESS_HELPER=1")
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	return p, func() int {
		pid, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0]))
		return pid
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// A killed child of this test binary lingers as a zombie until reaped;
	// a reparented one is reaped by init. Either way it is not running.
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return len(out) > 0 && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d outlived its launch", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCloseReapsWhatTheLaunchLeftBehind(t *testing.T) {
	p, pid := escapee(t, context.Background(), "exit 0")
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	escaped := pid()
	if escaped == 0 {
		t.Fatal("no escapee pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	own, _ := syscall.Getpgid(escaped)
	for deadline := time.Now().Add(3 * time.Second); own != escaped && time.Now().Before(deadline); own, _ = syscall.Getpgid(escaped) {
		time.Sleep(20 * time.Millisecond)
	}
	if !alive(escaped) || own != escaped {
		t.Fatalf("the escapee is not running in a group of its own (group %d)", own)
	}
	p.Close()
	waitGone(t, escaped)
}

func TestStopReapsDescendantsOutsideTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, pid := escapee(t, ctx, "sleep 60")
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	deadline := time.Now().Add(3 * time.Second)
	var escaped int
	for escaped == 0 && time.Now().Before(deadline) {
		escaped = pid()
		time.Sleep(20 * time.Millisecond)
	}
	if escaped == 0 {
		t.Fatal("no escapee pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	cancel()
	<-done
	p.Close()
	waitGone(t, escaped)
}

// Only processes carrying this launch's token are touched: an unrelated
// process, even a later one of this user's, survives.
func TestSweepSparesUnmarkedProcesses(t *testing.T) {
	p, _ := escapee(t, context.Background(), "exit 0")
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	bystander := exec.Command("sleep", "60")
	bystander.Env = os.Environ()
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })
	p.Close()
	if !alive(bystander.Process.Pid) {
		t.Fatal("the sweep killed an unmarked process")
	}
}

func TestLaunchTokensNest(t *testing.T) {
	outer := markedEnvironment([]string{"A=1"}, "outer")
	inner := markedEnvironment(outer, "inner")
	if !carriesToken(inner, "outer") || !carriesToken(inner, "inner") || carriesToken(outer, "inner") {
		t.Fatalf("outer %q inner %q", outer, inner)
	}
	if strings.Count(strings.Join(inner, "\n"), launchVariable+"=") != 1 {
		t.Fatalf("launch variable repeated: %q", inner)
	}
}

func TestSweepReachesUnreadableChildrenOfMarkedProcesses(t *testing.T) {
	cmd, p, err := Command(context.Background(), "/bin/sh", "-c", `"$0" -test.run='^TestHelper$' -- sleeper & sleep 1; exit 0`, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "HARNESS_PROCESS_HELPER=1")
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	_ = p.Run()
	sleeper, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("no sleeper pid in %q", out.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(sleeper, syscall.SIGKILL) })
	if !alive(sleeper) {
		t.Fatal("the sleeper is not running")
	}
	p.Close()
	waitGone(t, sleeper)
}

func niceOf(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "nice=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// The tree runs niced, including a descendant forked later into a process
// group of its own, as an agent's background server is.
func TestBackgroundLowersTheWholeTree(t *testing.T) {
	cmd, p, err := Command(context.Background(), "/bin/sh", "-c", `sleep 0.3; "$0" -test.run='^TestHelper$' -- detached >/dev/null 2>&1 & echo $!; sleep 1`, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "HARNESS_PROCESS_HELPER=1")
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	p.Background()
	started := make(chan int, 1)
	p.Notify(func(pid int) { started <- pid })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	deadline := time.Now().Add(3 * time.Second)
	var child int
	for child == 0 && time.Now().Before(deadline) {
		child, _ = strconv.Atoi(strings.TrimSpace(out.String()))
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("no child pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if got := niceOf(t, <-started); got != "10" {
		t.Errorf("leader nice %s", got)
	}
	if got := niceOf(t, child); got != "10" {
		t.Errorf("later descendant nice %s", got)
	}
	<-done
	p.Close()
}
