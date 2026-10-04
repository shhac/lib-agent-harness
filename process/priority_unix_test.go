//go:build darwin || linux

package process

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// The tree runs niced, including a descendant forked later into a process
// group of its own, as an agent's background server is.
func TestBackgroundLowersTheWholeTree(t *testing.T) {
	parentNice, err := observedNice(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("os=%s arch=%s go=%s parent_pid=%d parent_nice=%d", runtime.GOOS, runtime.GOARCH, runtime.Version(), os.Getpid(), parentNice)
	testenv.RequireProcessGroup(t)
	testenv.RequireGroupPriority(t)
	testenv.RequireProcessStatus(t)
	for _, mode := range []string{"original-shell", "controlled-shell", "direct"} {
		t.Run(mode, func(t *testing.T) {
			backgroundTree(t, mode)
		})
	}
	if nice, err := observedNice(os.Getpid()); err != nil || nice != parentNice {
		t.Errorf("parent priority changed: before=%d after=%d error=%v", parentNice, nice, err)
	}
}

// The helper reports only after leaving the launch group. The leader waits
// for an explicit release before forking, so the descendant is demonstrably
// admitted after Notify, rather than merely assumed to be later by a sleep.
func TestBackgroundHelper(t *testing.T) {
	if os.Getenv("HARNESS_BACKGROUND_HELPER") != "1" {
		return
	}
	if os.Args[len(os.Args)-1] == "detached" {
		if err := syscall.Setpgid(0, 0); err != nil {
			os.Exit(2)
		}
		nice, err := observedNice(os.Getpid())
		if err != nil {
			os.Exit(3)
		}
		fmt.Printf("%d %d\n", os.Getpid(), nice)
		_ = os.Stdout.Close()
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	input := bufio.NewScanner(os.Stdin)
	if !input.Scan() {
		os.Exit(4)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestBackgroundHelper$", "--", "detached")
	child.Env = os.Environ()
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(5)
	}
	_ = input.Scan()
	_ = child.Process.Kill()
	_ = child.Wait()
	os.Exit(0)
}

// Darwin's libc returns nice directly; Linux's syscall returns 20 - nice.
// Narrow libc's signed C int explicitly, including negative inherited nice.
func observedNice(pid int) (int, error) {
	value, err := syscall.Getpriority(syscall.PRIO_PROCESS, pid)
	if runtime.GOOS == "linux" {
		value = 20 - value
	} else {
		value = int(int32(value))
	}
	return value, err
}

func backgroundTree(t *testing.T, mode string) {
	t.Helper()
	args := []string{"-test.run=^TestBackgroundHelper$", "--", "leader"}
	name := os.Args[0]
	if mode == "controlled-shell" {
		name = "/bin/sh"
		args = []string{"-c", `read _; "$0" -test.run='^TestBackgroundHelper$' -- detached & read _`, os.Args[0]}
	}
	if mode == "original-shell" {
		// Retain the original startup/fork path, including its early sleep
		// child. Only the final sleep becomes a pipe wait to hold the leader
		// alive during inspection. This path must not be hidden by the new
		// controlled fixtures postponing all forks until after Notify.
		name = "/bin/sh"
		args = []string{"-c", `sleep 0.3; "$0" -test.run='^TestHelper$' -- detached >/dev/null 2>&1 & echo $!; read _`, os.Args[0]}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, p, err := Command(ctx, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	// A file-backed pipe avoids an exec input-copy goroutine that can remain
	// blocked on a test-owned reader after cancellation.
	input, release, err := os.Pipe()
	if err != nil {
		p.Close()
		t.Fatal(err)
	}
	defer input.Close()
	defer release.Close()
	cmd.Stdin = input
	cmd.Env = append(os.Environ(), "HARNESS_BACKGROUND_HELPER=1", "HARNESS_PROCESS_HELPER=1")
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second
	p.Background()
	type observation struct {
		pid, nice int
		birth     string
		err       error
	}
	started := make(chan observation, 1)
	p.Notify(func(pid int) {
		nice, err := observedNice(pid)
		started <- observation{pid: pid, nice: nice, birth: processIdentity(pid), err: err}
	})
	done := make(chan error, 1)
	var runErr error
	settled := false
	// Register cleanup before starting any work, including fatal assertions.
	t.Cleanup(func() {
		_ = release.Close()
		p.Close()
		if !settled {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("background Run did not settle during cleanup")
			}
		}
	})
	go func() { done <- p.Run() }()
	var leader observation
	select {
	case leader = <-started:
	case runErr = <-done:
		settled = true
		t.Fatalf("Run ended before Notify: %v; output=%q", runErr, out.String())
	case <-ctx.Done():
		t.Fatal("timed out waiting for Notify")
	}
	if leader.birth != "" {
		checkBackgroundObservation(t, leader.pid, leader.birth)
	}
	if leader.err != nil || leader.nice != backgroundNice || leader.birth == "" {
		t.Fatalf("leader at Notify: pid=%d birth=%q nice=%d error=%v", leader.pid, leader.birth, leader.nice, leader.err)
	}
	if mode != "original-shell" {
		if _, err := release.Write([]byte("fork\n")); err != nil {
			t.Fatal(err)
		}
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var child, selfNice int
		if strings.Contains(out.String(), "\n") {
			var parseErr error
			if mode == "original-shell" {
				_, parseErr = fmt.Sscanf(out.String(), "%d", &child)
			} else {
				_, parseErr = fmt.Sscanf(out.String(), "%d %d", &child, &selfNice)
			}
			if parseErr != nil || child <= 1 {
				t.Fatalf("invalid detached readiness: %q", out.String())
			}
			birth := processIdentity(child)
			group, err := syscall.Getpgid(child)
			if err != nil || birth == "" {
				t.Fatalf("detached readiness: pid=%d birth=%q group=%d error=%v", child, birth, group, err)
			}
			if group == child {
				if mode != "original-shell" && selfNice != backgroundNice {
					t.Errorf("descendant self-reported nice=%d, want %d", selfNice, backgroundNice)
				}
				checkBackgroundObservation(t, child, birth)
				checkBackgroundObservation(t, leader.pid, leader.birth)
				return
			}
			if mode != "original-shell" {
				t.Fatalf("helper reported readiness before detachment: pid=%d group=%d", child, group)
			}
		}
		select {
		case runErr = <-done:
			settled = true
			t.Fatalf("Run ended before detached readiness: %v; output=%q", runErr, out.String())
		case <-ctx.Done():
			t.Fatalf("timed out waiting for detached readiness; output=%q", out.String())
		case <-tick.C:
		}
	}
}

// Compare independent syscall and ps observations while the fixture holds
// both processes alive, bracketing ps with birth checks to reject PID reuse.
func checkBackgroundObservation(t *testing.T, pid int, birth string) {
	t.Helper()
	if processIdentity(pid) != birth {
		t.Fatalf("pid %d identity changed before priority observation", pid)
	}
	nice, err := observedNice(pid)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, psErr := exec.CommandContext(ctx, "ps", "-o", "pid=,ppid=,pgid=,nice=", "-p", strconv.Itoa(pid)).Output()
	var reportedPID, parent, group, psNice int
	_, parseErr := fmt.Sscanf(string(out), "%d %d %d %d", &reportedPID, &parent, &group, &psNice)
	if processIdentity(pid) != birth || err != nil || psErr != nil || parseErr != nil || reportedPID != pid || nice != backgroundNice || psNice != backgroundNice {
		t.Errorf("priority observation: pid=%d birth=%q syscall_nice=%d syscall_error=%v ps=%q ps_error=%v parse_error=%v; want nice=%d", pid, birth, nice, err, out, psErr, parseErr, backgroundNice)
	}
}
