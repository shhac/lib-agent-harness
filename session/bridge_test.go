//go:build !windows

package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These env names make the test binary re-execute itself as a stand-in for a
// bridge running under a harness: it takes the lock and waits to be killed.
const (
	holdLockEnv   = "AGENT_HARNESS_TEST_HOLD_LOCK"
	holdLaunchEnv = "AGENT_HARNESS_TEST_HOLD_LAUNCH"
)

func TestMain(m *testing.M) {
	if path := os.Getenv(holdLockEnv); path != "" {
		lock, err := holdBridgeLock(path, os.Getenv(holdLaunchEnv))
		if err != nil {
			os.Exit(2)
		}
		defer lock.Close()
		if _, err = os.Stdout.WriteString("held\n"); err != nil {
			os.Exit(2)
		}
		select {}
	}
	os.Exit(m.Run())
}

// A bridge relays the harness's protocol and proves it holds this session's
// channel credential. It carries no policy and interprets nothing.
func TestRunBridgeRelaysAuthenticatedTraffic(t *testing.T) {
	h := testHost(t, echoHandler(t))
	for key, value := range h.environment() {
		t.Setenv(key, value)
	}
	harnessIn, bridgeIn := io.Pipe()
	bridgeOut, harnessOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunBridge(ctx, harnessIn, harnessOut) }()

	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_file", "arguments": map[string]any{"path": "a.go"}}})
	if _, err := bridgeIn.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bridgeOut)
	if !scanner.Scan() {
		t.Fatalf("bridge relayed no reply: %v", scanner.Err())
	}
	if !strings.Contains(scanner.Text(), `\"path\":\"a.go\"`) {
		t.Fatalf("bridge did not relay the tool result: %s", scanner.Text())
	}
	held, err := readBridgeLock(lockPath(h.cfg.Dir))
	if err != nil || held == nil {
		t.Fatalf("a running bridge did not hold its lock: %v %v", held, err)
	}
	// The launch identity is what lets recovery tell this bridge apart from an
	// unrelated process that inherited the same group number.
	if held.Launch != h.socketDir {
		t.Errorf("bridge did not record the launch it belongs to: %+v", held)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not stop with its context")
	}
}

func TestRunBridgeRefusesWithoutASession(t *testing.T) {
	t.Setenv(BridgeSocketEnv, "")
	t.Setenv(BridgeSecretEnv, "")
	t.Setenv(BridgeLockEnv, "")
	if err := RunBridge(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("a bridge started outside a session")
	}
}

func TestReclaimReportsNothingWhenNothingWasLaunched(t *testing.T) {
	out, err := Reclaim(context.Background(), privateDir(t))
	if err != nil || out.Found || !out.Confirmed {
		t.Fatalf("an absent launch record was not reported as nothing to reclaim: %+v %v", out, err)
	}
}

// A free bridge lock is not evidence of anything. A harness can outlive its tool
// server, restart it, or sit in inference with none running, so absence has to
// come from the process group and identity from a live bridge.
func TestFreeBridgeLockIsNotProofTheHarnessIsGone(t *testing.T) {
	dir := privateDir(t)
	group, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	// A recorded group that is certainly alive — this test's own — with no bridge
	// holding the lock.
	if err = recordLaunch(dir, launchRecord{Engine: "claude", PID: os.Getpid(), Group: group, Launch: "/tmp/absent"}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(lockPath(dir), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := Reclaim(context.Background(), dir)
	if !errors.Is(err, ErrUnreclaimed) {
		t.Fatalf("a free lock over a live group was read as absence: %+v %v", out, err)
	}
	if !out.Found || out.Confirmed || out.Terminated {
		t.Fatalf("unresolved recovery was reported as settled: %+v", out)
	}
}

// A group with no members is the one thing that does establish absence.
func TestReclaimConfirmsAbsenceFromTheProcessGroup(t *testing.T) {
	dir := privateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestReclaimConfirmsAbsenceFromTheProcessGroup")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+filepath.Join(privateDir(t), "unused.lock"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := recordLaunch(dir, launchRecord{Engine: "claude", PID: pid, Group: pid, Launch: "/tmp/gone"}); err != nil {
		t.Fatal(err)
	}
	out, err := Reclaim(context.Background(), dir)
	if err != nil || out.Found || !out.Confirmed {
		t.Fatalf("a dead group was not confirmed absent: %+v %v", out, err)
	}
}

// A live group whose bridge names the same launch is provably ours, so it can be
// signalled — and termination is confirmed from the group, not from the lock.
func TestReclaimTerminatesAnIdentifiedOrphan(t *testing.T) {
	dir := privateDir(t)
	launch := filepath.Join(privateDir(t), "launch")
	cmd := exec.Command(os.Args[0], "-test.run=TestReclaimTerminatesAnIdentifiedOrphan")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+lockPath(dir), holdLaunchEnv+"="+launch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap as soon as it dies. In the situation this models, the process that
	// started the harness is gone and init reaps it; here the test is still its
	// parent, and an unreaped child stays visible to a liveness probe.
	reaped := make(chan struct{})
	go func() { defer close(reaped); _ = cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill(); <-reaped }()
	ready := bufio.NewScanner(stdout)
	if !ready.Scan() || ready.Text() != "held" {
		t.Fatalf("stand-in bridge did not take the lock: %v", ready.Err())
	}
	if err = recordLaunch(dir, launchRecord{Engine: "claude", PID: cmd.Process.Pid, Group: cmd.Process.Pid, Launch: launch}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := Reclaim(ctx, dir)
	if err != nil {
		t.Fatalf("reclaim failed: %v", err)
	}
	if !out.Found || !out.Terminated || !out.Confirmed || out.Group != cmd.Process.Pid {
		t.Fatalf("identified orphan was not reclaimed: %+v", out)
	}
}

// A live group whose bridge belongs to a different launch must never be
// signalled: that is exactly the identifier-reuse case.
func TestReclaimRefusesAnUnidentifiedLiveGroup(t *testing.T) {
	dir := privateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestReclaimRefusesAnUnidentifiedLiveGroup")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+lockPath(dir), holdLaunchEnv+"=/tmp/some-other-launch")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := bufio.NewScanner(stdout)
	if !ready.Scan() || ready.Text() != "held" {
		t.Fatalf("stand-in bridge did not take the lock: %v", ready.Err())
	}
	if err = recordLaunch(dir, launchRecord{Engine: "claude", PID: cmd.Process.Pid, Group: cmd.Process.Pid, Launch: "/tmp/our-launch"}); err != nil {
		t.Fatal(err)
	}
	out, err := Reclaim(context.Background(), dir)
	if !errors.Is(err, ErrUnreclaimed) {
		t.Fatalf("a group with foreign identity was accepted: %+v %v", out, err)
	}
	if out.Terminated {
		t.Fatal("a process group was signalled without identity proof")
	}
	if cmd.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("the unidentified process was killed")
	}
}

// Recovery must never signal the group it is running in, and must never treat an
// unusable record as a licence to kill something.
func TestRecoveryNeverSignalsImplausibleGroups(t *testing.T) {
	if terminateGroup(0, 0) == nil || terminateGroup(1, 0) == nil {
		t.Error("an implausible process group was accepted for termination")
	}
	own, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if terminateGroup(own, 0) == nil {
		t.Error("this process's own group was accepted for termination")
	}
	if _, err = groupAlive(1); err == nil {
		t.Error("an implausible group was probed rather than refused")
	}
	dir := privateDir(t)
	unusable, _ := json.Marshal(launchRecord{Engine: "claude", Group: 0})
	if err = os.WriteFile(launchPath(dir), unusable, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Reclaim(context.Background(), dir); err == nil {
		t.Error("an unusable launch record was accepted")
	}
}

// Two processes must not drive one assignment, including before any bridge has
// started.
func TestAssignmentLeaseExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(privateDir(t), "session.lease")
	first, err := holdLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holdLease(path); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a second holder took the assignment lease: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := holdLease(path)
	if err != nil {
		t.Fatalf("lease was not released: %v", err)
	}
	_ = second.Close()
}
