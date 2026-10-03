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

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// These env names make the test binary re-execute itself as a stand-in for a
// bridge running under a harness: it takes the lock and waits to be killed.
const (
	holdLockEnv      = "AGENT_HARNESS_TEST_HOLD_LOCK"
	holdLaunchEnv    = "AGENT_HARNESS_TEST_HOLD_LAUNCH"
	settleFixtureEnv = "AGENT_HARNESS_TEST_SETTLE"
)

// holdFixtureLifetime bounds a stand-in bridge that was never killed. It is far
// longer than any test that starts one, so reaching it means something went
// wrong rather than that a test was slow.
const holdFixtureLifetime = 10 * time.Minute

func TestMain(m *testing.M) {
	if mode := os.Getenv(settleFixtureEnv); mode != "" {
		var lock *os.File
		if mode == "owner" || mode == "lock-exit" {
			var err error
			lock, err = os.OpenFile(os.Getenv(holdLockEnv), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
			if err != nil || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
				os.Exit(2)
			}
		}
		_, _ = os.Stdout.WriteString("ready\n")
		if mode == "survivor" {
			time.Sleep(holdFixtureLifetime)
			os.Exit(0)
		}
		time.Sleep(300 * time.Millisecond)
		if mode == "owner" {
			group, _ := syscall.Getpgid(0)
			raw, _ := json.Marshal(owner{Group: group, PID: os.Getpid(), Launch: os.Getenv(holdLaunchEnv)})
			if _, err := lock.WriteAt(raw, 0); err != nil {
				os.Exit(2)
			}
			time.Sleep(holdFixtureLifetime)
		}
		os.Exit(0)
	}
	if scenario := os.Getenv(fakeScenarioEnv); scenario != "" {
		os.Exit(runFakeHarness(scenario))
	}
	if path := os.Getenv(holdLockEnv); path != "" {
		lock, err := holdBridgeLock(path, os.Getenv(holdLaunchEnv))
		if err != nil {
			os.Exit(2)
		}
		defer lock.Close()
		if _, err = os.Stdout.WriteString("held\n"); err != nil {
			os.Exit(2)
		}
		// Wait to be killed, by sleeping rather than by blocking forever.
		//
		// `select {}` is not a way to stay alive: with nothing else runnable the
		// Go runtime declares a deadlock and exits the process. This fixture
		// exists to *be* an orphan, so a fixture that sometimes exited on its own
		// made the tests that look for one flaky — on a loaded CI machine the
		// orphan was already gone before Reclaim went looking.
		//
		// The bound is a backstop: a fixture leaked by a test that failed before
		// its kill must not outlive the run that started it.
		time.Sleep(holdFixtureLifetime)
		os.Exit(3)
	}
	os.Exit(m.Run())
}

// requireAlive fails the test if the stand-in bridge is not running. A dead
// fixture makes every assertion after it meaningless, and saying so here is the
// difference between "the fixture exited" and "Reclaim reported the wrong thing".
func requireAlive(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("stand-in bridge %d could not be found: %v", pid, err)
	}
	if err = process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("stand-in bridge %d exited before the test could use it: %v", pid, err)
	}
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
	grace := reclaimGrace
	reclaimGrace = 150 * time.Millisecond
	t.Cleanup(func() { reclaimGrace = grace })
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
	testenv.RequireProcessGroup(t)
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
	testenv.RequireProcessGroup(t)
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
	// What follows is only meaningful against a live orphan. Checking here
	// separates a fixture that died from a Reclaim that got the wrong answer.
	requireAlive(t, cmd.Process.Pid)
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
	grace := reclaimGrace
	reclaimGrace = 150 * time.Millisecond
	t.Cleanup(func() { reclaimGrace = grace })
	testenv.RequireProcessGroup(t)
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
	requireAlive(t, cmd.Process.Pid)
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

// standInBridge starts a live process in its own group, holding the bridge lock
// for launch the way a running harness's bridge does, and returns its pid.
func standInBridge(t *testing.T, lock, launch string) int {
	t.Helper()
	testenv.RequireProcessGroup(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+lock, holdLaunchEnv+"="+launch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { defer close(reaped); _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })
	ready := bufio.NewScanner(stdout)
	if !ready.Scan() || ready.Text() != "held" {
		t.Fatalf("stand-in bridge did not take the lock: %v", ready.Err())
	}
	requireAlive(t, cmd.Process.Pid)
	return cmd.Process.Pid
}

// Release gives up the assignment lease when it closes, and another session may
// take it and launch before Release gets round to reclaiming. What Release then
// finds on disk is that session's live harness, and it must be left alone.
func TestReleaseLeavesAnAssignmentAnotherSessionTook(t *testing.T) {
	dir := hostDir(t)
	o, err := normalize(Options{
		Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: "/usr/bin/true", Home: t.TempDir()}}, WorkDir: t.TempDir(), RuntimeHome: t.TempDir(),
		Restriction: &Restriction{Tools: ToolHost{
			Server: "agent_workspace", Handler: echoHandler(t), Dir: dir,
			Bridge: Bridge{Path: "/usr/bin/true"},
			Tools:  []ToolDefinition{{Name: "read_file", Schema: map[string]any{"type": "object"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := newToolHost(o.Restriction.Tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := &Session{options: o, tools: host, done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	// The released session's harness has ended and its lease is free. Another
	// session takes the lease and launches a harness of its own.
	host.close()
	lease, err := holdLease(leasePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	launch := filepath.Join(privateDir(t), "launch")
	live := standInBridge(t, lockPath(dir), launch)
	if err = recordLaunch(dir, launchRecord{Engine: "claude", PID: live, Group: live, Launch: launch}); err != nil {
		t.Fatal(err)
	}

	out, err := released.Release(context.Background())
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("release did not report the assignment held elsewhere: %+v %v", out, err)
	}
	if out.Terminated || out.Confirmed {
		t.Fatalf("release reclaimed an assignment it no longer held: %+v", out)
	}
	requireAlive(t, live)
	if _, err = os.Stat(launchPath(dir)); err != nil {
		t.Fatal("release cleared another session's launch record")
	}
}

// A launch record that cannot be read says a launch happened and nothing about
// what it left running, so the assignment is reserved like any other harness
// that cannot be confirmed gone.
func TestUnreadableLaunchRecordIsUnreclaimed(t *testing.T) {
	dir := privateDir(t)
	if err := os.WriteFile(launchPath(dir), []byte(`{"engine":"claude","pid":`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := Reclaim(context.Background(), dir)
	if !errors.Is(err, ErrUnreclaimed) {
		t.Fatalf("an unreadable launch record was not reported as unreclaimed: %+v %v", out, err)
	}
	if out.Confirmed {
		t.Fatalf("an unreadable launch record was confirmed absent: %+v", out)
	}
}

// A launch record is replaced whole, never rewritten in place: a crash midway
// through the rewrite that names the running harness must leave the earlier
// record, not an empty one, and a link planted at the record's path is
// replaced rather than written through.
func TestLaunchRecordIsReplacedWhole(t *testing.T) {
	dir := privateDir(t)
	elsewhere := filepath.Join(privateDir(t), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, launchPath(dir)); err != nil {
		t.Fatal(err)
	}
	if err := recordLaunch(dir, launchRecord{Engine: "claude", PID: 999999, Group: 999999, Launch: dir}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(elsewhere); err != nil || string(raw) != "untouched" {
		t.Fatalf("the launch record was written through a link: %q %v", raw, err)
	}
	info, err := os.Lstat(launchPath(dir))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("the launch record is not an owner-only file: %v %v", info, err)
	}
	record, err := readLaunchRecord(dir)
	if err != nil || record == nil || record.Group != 999999 {
		t.Fatalf("the launch record did not round-trip: %+v %v", record, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("replacing the launch record left other files behind: %v", entries)
	}
}

// A test-owned child makes the otherwise scheduler-dependent orphan reap window
// deterministic: death releases flock, but Wait is deliberately delayed.
func TestReclaimSettlement(t *testing.T) {
	testenv.RequireProcessGroup(t)
	for _, mode := range []string{"exit", "lock-exit", "owner", "survivor", "unreaped", "unreaped-timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := privateDir(t)
			grace := reclaimGrace
			// Race-instrumented fixtures also wait during runtime shutdown.
			reclaimGrace = 3 * time.Second
			if mode == "survivor" || mode == "unreaped-timeout" {
				reclaimGrace = 150 * time.Millisecond
			}
			defer func() { reclaimGrace = grace }()
			fixture := mode
			if mode == "unreaped" || mode == "unreaped-timeout" || mode == "cancel" {
				fixture = "survivor"
			}
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), settleFixtureEnv+"="+fixture, holdLockEnv+"="+lockPath(dir), holdLaunchEnv+"="+dir)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := bufio.NewScanner(stdout)
			if !ready.Scan() || ready.Text() != "ready" {
				t.Fatal("fixture not ready")
			}
			pid := cmd.Process.Pid
			if err := recordLaunch(dir, launchRecord{PID: pid, Group: pid, Launch: dir}); err != nil {
				t.Fatal(err)
			}
			reaped := make(chan struct{})
			delayed := mode == "unreaped" || mode == "unreaped-timeout"
			if delayed {
				_ = cmd.Process.Kill()
				// Some kernels omit zombies from kill(-pgid, 0). Only test
				// the unreaped window on systems where it is observable.
				time.Sleep(50 * time.Millisecond)
				alive, err := groupAlive(pid)
				if err != nil || !alive {
					_ = cmd.Wait()
					t.Skip("kernel does not count this unreaped group")
				}
			}
			go func() {
				defer close(reaped)
				if delayed {
					time.Sleep(300 * time.Millisecond)
				}
				_ = cmd.Wait()
			}()
			defer func() { _ = cmd.Process.Kill(); <-reaped }()
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			start := time.Now()
			out, err := Reclaim(ctx, dir)
			refused := mode == "survivor" || mode == "unreaped-timeout" || mode == "cancel"
			if refused {
				if !errors.Is(err, ErrUnreclaimed) || !out.Found || out.Confirmed || out.Terminated {
					t.Fatalf("unsafe refusal: %+v %v", out, err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				if _, err := os.Stat(launchPath(dir)); err != nil {
					t.Fatal("marker lost")
				}
				if mode != "unreaped-timeout" {
					requireAlive(t, pid)
				}
			} else {
				if err != nil || !out.Found || !out.Confirmed || out.Terminated != (mode == "owner") {
					t.Fatalf("not settled: %+v %v", out, err)
				}
				if time.Since(start) < 200*time.Millisecond {
					t.Fatal("did not wait for settlement")
				}
			}
			if mode == "unreaped-timeout" {
				<-reaped
				out, err = Reclaim(context.Background(), dir)
				if err != nil || !out.Confirmed {
					t.Fatalf("later recovery: %+v %v", out, err)
				}
			}
		})
	}
}

func TestReleaseWaitsForLateGroupReaping(t *testing.T) {
	testenv.RequireProcessGroup(t)
	for _, survivor := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "survivor"}[survivor], func(t *testing.T) {
			o, _ := probedOptions(t, harness.Claude, "late-release")
			ctx := probeContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			dir := o.Restriction.Tools.Dir
			record, err := readLaunchRecord(dir)
			if err != nil || record == nil {
				t.Fatal("missing record")
			}
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), settleFixtureEnv+"=survivor")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: record.Group}
			pipe, err := cmd.StdoutPipe()
			if err != nil || cmd.Start() != nil {
				t.Fatal("cannot join group")
			}
			ready := bufio.NewScanner(pipe)
			if !ready.Scan() {
				t.Fatal("member not ready")
			}
			reaped := make(chan struct{})
			reap := make(chan struct{})
			go func() { <-reap; _ = cmd.Wait(); close(reaped) }()
			defer func() { _ = cmd.Process.Kill(); <-reaped }()
			grace := reclaimGrace
			if survivor {
				reclaimGrace = 150 * time.Millisecond
			}
			defer func() { reclaimGrace = grace }()
			if !survivor {
				time.AfterFunc(time.Second, func() { close(reap) })
			}
			out, err := s.Release(ctx)
			if survivor {
				close(reap)
				if !errors.Is(err, ErrUnreclaimed) || out.Confirmed {
					t.Fatalf("accepted survivor: %+v %v", out, err)
				}
				if _, err := os.Stat(launchPath(dir)); err != nil {
					t.Fatal("marker lost")
				}
				<-reaped
				out, err = Reclaim(ctx, dir)
				if err != nil || !out.Confirmed {
					t.Fatalf("recovery failed: %+v %v", out, err)
				}
			} else {
				if err != nil || !out.Confirmed {
					t.Fatalf("release failed: %+v %v", out, err)
				}
				if _, err := os.Stat(launchPath(dir)); !os.IsNotExist(err) {
					t.Fatal("marker remains")
				}
			}
			if alive, err := groupAlive(record.Group); err != nil || alive {
				t.Fatalf("group remains: %v %v", alive, err)
			}
		})
	}
}

func TestReclaimKeepsTheLeaseWhileWaiting(t *testing.T) {
	dir := privateDir(t)
	group, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLaunch(dir, launchRecord{PID: os.Getpid(), Group: group, Launch: dir}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		lease, _, err := reclaimUnderLease(ctx, dir)
		if lease != nil {
			_ = lease.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		lease, err := holdLease(leasePath(dir))
		if errors.Is(err, ErrLeaseHeld) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = lease.Close()
		if time.Now().After(deadline) {
			t.Fatal("reclamation did not take the lease")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrUnreclaimed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	lease, err := holdLease(leasePath(dir))
	if err != nil {
		t.Fatalf("failed wait retained the lease: %v", err)
	}
	_ = lease.Close()
	if _, err := os.Stat(launchPath(dir)); err != nil {
		t.Fatal("failed wait lost the marker")
	}
}
