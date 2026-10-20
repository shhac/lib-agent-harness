//go:build !windows

package process

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// While the leader's PID names a different process (simulated by a birth
// identity that no longer matches), reapGroup must not signal its group,
// which could be someone else's: a member only a group signal can reach
// survives. Once the leader is gone its identity is unknown rather than
// different, and a live member anchors the group, so stopping it then reaps
// the member legitimately.
func TestReusedLeaderGroupIsNeverSignalled(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	// The member is double-forked out of the leader's tree and carries no
	// marker, so only a signal to the leader's group would reach it.
	cmd, p, err := Command(context.Background(), "/bin/sh", "-c", `( env -i /bin/sleep 30 & echo $! ); exec /bin/sleep 30`)
	if err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	started := make(chan int, 1)
	p.Notify(func(pid int) { started <- pid })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	leader := <-started
	var member int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && member <= 1; time.Sleep(10 * time.Millisecond) {
		member, _ = strconv.Atoi(strings.TrimSpace(out.String()))
	}
	if member <= 1 {
		t.Fatal("no group member started")
	}
	t.Cleanup(func() { _ = syscall.Kill(member, syscall.SIGKILL) })
	if group, err := syscall.Getpgid(member); err != nil || group != leader {
		t.Fatalf("member is not in the leader's group: %d %v", group, err)
	}

	p.mu.Lock()
	p.leaderIdentity = "a-process-that-is-gone"
	p.mu.Unlock()
	p.reapGroup()
	if !alive(member) {
		t.Fatal("reapGroup signalled a reused leader's group")
	}
	p.mu.Lock()
	p.leaderIdentity = processIdentity(leader)
	p.mu.Unlock()
	p.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the leader was not stopped")
	}
	waitGone(t, member)
}

// GroupHasChildren answers only for the leader it started: true while a
// member lives, an error (never false) once the leader is gone or its
// identity no longer matches, so a caller cannot read lost evidence as "the
// background job exited".
func TestGroupHasChildrenAnswersOnlyForItsLeader(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	cmd, p, err := Command(context.Background(), "/bin/sh", "-c", `/bin/sleep 30 & echo $!; exec /bin/sleep 30`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GroupHasChildren(); err == nil {
		t.Fatal("a process not yet started answered")
	}
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	started := make(chan int, 1)
	p.Notify(func(pid int) { started <- pid })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	leader := <-started
	var member int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && member <= 1; time.Sleep(10 * time.Millisecond) {
		member, _ = strconv.Atoi(strings.TrimSpace(out.String()))
	}
	t.Cleanup(func() { _ = syscall.Kill(member, syscall.SIGKILL) })
	if has, err := p.GroupHasChildren(); err != nil || !has {
		t.Fatalf("a live member was not reported: %v %v", has, err)
	}
	p.mu.Lock()
	real := p.leaderIdentity
	p.leaderIdentity = "a-process-that-is-gone"
	p.mu.Unlock()
	if _, err := p.GroupHasChildren(); err == nil {
		t.Fatal("a changed leader identity answered")
	}
	p.mu.Lock()
	p.leaderIdentity = real
	p.mu.Unlock()
	p.Close()
	<-done
	if _, err := p.GroupHasChildren(); err == nil {
		t.Fatalf("a finished leader %d answered", leader)
	}
}
