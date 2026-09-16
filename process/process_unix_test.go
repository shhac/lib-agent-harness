//go:build unix

package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDetachFromTerminalSignalsLeavesOurProcessGroup pins the property that
// keeps a Ctrl-C from killing a review that is minutes and a million tokens
// in: the engine subprocess must NOT share our process group, because a
// terminal delivers SIGINT to the whole foreground group and a child inherits
// its parent's group by default.
//
// The assertion is on the group id rather than on an actual signal, because
// signalling our own group is what a terminal does and would take the test
// binary down with it. The contrast case is asserted too: without the helper
// the child DOES share our group, so this test fails if the helper silently
// stops doing anything, rather than passing for the wrong reason.
func TestDetachFromTerminalSignalsLeavesOurProcessGroup(t *testing.T) {
	ours, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid(self): %v", err)
	}

	start := func(detach bool) int {
		t.Helper()
		// CommandContext, as every production call site uses.
		cmd := exec.CommandContext(t.Context(), "sleep", "30")
		if detach {
			prepareTestProcess(t, cmd)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("Getpgid(child): %v", err)
		}
		return pgid
	}

	if pgid := start(true); pgid == ours {
		t.Errorf("detached child is in our process group (%d); a terminal Ctrl-C would kill it mid-review", pgid)
	}
	// Without the helper the child shares our group. If this ever stops being
	// true the assertion above proves nothing, so it is checked rather than
	// assumed.
	if pgid := start(false); pgid != ours {
		t.Errorf("undetached child pgid = %d, want ours (%d); the test above is no longer meaningful", pgid, ours)
	}
}

func prepareTestProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	p, err := New(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}

// Checking pipe-drain timing alone would pass if WaitDelay returned while a
// grandchild still lived. Inspect the descendant too, allowing dead zombies
// which Linux containers may leave for their init process to reap.
func TestCancelledGroupStopsDescendant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p, buf := helper(t, ctx, "parent")
	if p.Run() == nil {
		t.Fatal("cancelled parent succeeded")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(buf.String()))
	if err != nil {
		t.Fatalf("missing child pid: %q", buf.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		status, e := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if e != nil && len(bytes.TrimSpace(status)) == 0 {
			return
		}
		if strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("descendant %d survived group cancellation (status %q)", pid, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
