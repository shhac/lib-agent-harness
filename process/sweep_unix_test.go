//go:build darwin || linux

package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// escapee runs a shell that backgrounds a helper, which moves itself into a
// process group of its own, and exits, as Claude Code does with a background
// command: the helper is reparented and outside the contained group. The
// helper is this test binary, not sleep: macOS hides the environment of its
// own platform binaries.
func escapee(t *testing.T, ctx context.Context, linger string) (*Process, func() int) {
	t.Helper()
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
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

// alive needs ps: a test that calls it first calls testenv.RequireProcessStatus,
// since a refused ps would make every process look gone.
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

func TestStopKillsGroupAfterLeaderExit(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	for _, unknown := range []bool{false, true} {
		t.Run(strconv.FormatBool(unknown), func(t *testing.T) {
			cmd, p, err := Command(context.Background(), "/bin/sh", "-c", "env -i /bin/sleep 30 & echo $!; exit 0")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			out := &lockedBuffer{}
			cmd.Stdout = out // child holds this pipe after Wait reaps the leader
			cmd.WaitDelay = 10 * time.Second
			started := make(chan int, 1)
			p.Notify(func(pid int) { started <- pid })
			done := make(chan error, 1)
			go func() { done <- p.Run() }()
			leader := <-started
			deadline := time.Now().Add(3 * time.Second)
			var child int
			for time.Now().Before(deadline) {
				child, _ = strconv.Atoi(strings.TrimSpace(out.String()))
				if child > 1 && processIdentity(leader) == "" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if child <= 1 || processIdentity(leader) != "" {
				t.Fatal("leader did not exit before pipe drain")
			}
			t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
			if !alive(child) {
				t.Fatal("group member did not survive leader")
			}
			if unknown {
				p.mu.Lock()
				p.leaderIdentity = ""
				p.mu.Unlock()
			}
			p.Stop()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("group held pipes after Stop")
			}
			waitGone(t, child)
		})
	}
}

func TestSweepReachesUnreadableChildrenOfMarkedProcesses(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
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
	testenv.RequireProcessGroup(t)
	testenv.RequireGroupPriority(t)
	testenv.RequireProcessStatus(t)
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

func TestRunReapsGroupWhenLeaderKilled(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	path := filepath.Join(t.TempDir(), "pid")
	_, p, err := Command(context.Background(), "/bin/sh", "-c", `env -i /bin/sleep 30 >/dev/null 2>&1 & echo $! > "$1"; kill -KILL $$`, "fixture", path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Run(); err == nil {
		t.Fatal("leader was not killed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatal("invalid child PID")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	waitGone(t, pid)
}

// Exercise final containment while an unmarked member keeps creating children.
// Environment sweeping cannot account for this fixture; the group must do it.
func TestCloseReapsForkingGroupMembers(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	cmd, p, err := Command(context.Background(), "/bin/sh", "-c",
		"env -i PATH=/usr/bin:/bin /bin/sh -c 'while :; do sleep 10 & sleep 0.01; done' >/dev/null 2>&1 & echo $!; exit 0")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	member, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	if !alive(member) {
		t.Fatal("forking member exited before Close")
	}
	p.Close()
	for _, proc := range candidates(p.launched.Add(-time.Second)) {
		if proc.group == cmd.Process.Pid && alive(proc.pid) {
			t.Fatalf("live group member %d survived Close", proc.pid)
		}
	}
}

// TestHiddenHelper keeps a marked parent in the launch group while its child
// starts in a detached group. No model or credentialed CLI is involved.
func TestHiddenHelper(t *testing.T) {
	if os.Getenv("HARNESS_HIDER") != "1" {
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestHelper$", "--", "wait")
	child.Env = append(os.Environ(), "HARNESS_PROCESS_HELPER=1", "HIDDEN_CHILD=1")
	if os.Getenv("HARNESS_PLATFORM_CHILD") == "1" {
		child = exec.Command("/bin/sleep", "60")
		child.Env = os.Environ()
	}
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("HARNESS_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
		os.Exit(2)
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

func hiddenLaunch(t *testing.T, ctx context.Context, ending string, platform bool) (*Process, <-chan error, int) {
	t.Helper()
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	path := filepath.Join(t.TempDir(), "child")
	script := `"$0" -test.run='^TestHiddenHelper$' >/dev/null 2>&1 & while [ ! -s "$1" ]; do sleep 0.01; done; ` + ending
	cmd, p, err := Command(ctx, "/bin/sh", "-c", script, os.Args[0], path)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "HARNESS_HIDER=1", "HARNESS_CHILD_PID="+path)
	if platform {
		cmd.Env = append(cmd.Env, "HARNESS_PLATFORM_CHILD=1")
	}
	t.Cleanup(p.Close)
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		raw, _ := os.ReadFile(path)
		pid, _ = strconv.Atoi(string(raw))
		if pid > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatal("hidden helper did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if group, err := syscall.Getpgid(pid); err == nil && group != pid {
		t.Fatalf("child group %d, want %d", group, pid)
	}
	return p, done, pid
}

func hideChildEnvironments(t *testing.T, hideParent bool) {
	t.Helper()
	old := readEnvironment.Load()
	readEnvironment.Store(func(pid int) []string {
		env := environment(pid)
		for _, value := range env {
			if value == "HIDDEN_CHILD=1" || (hideParent && value == "HARNESS_HIDER=1") {
				return nil
			}
		}
		return env
	})
	t.Cleanup(func() { readEnvironment.Store(old) })
}

func unmarkedBystanders(t *testing.T) func() {
	t.Helper()
	var pids []int
	for _, detached := range []bool{false, true} {
		cmd := exec.Command("/bin/sleep", "60")
		cmd.Env = os.Environ()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: detached}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		pids = append(pids, cmd.Process.Pid)
	}
	return func() {
		for _, pid := range pids {
			if !alive(pid) {
				t.Errorf("unmarked bystander %d was killed", pid)
			}
		}
	}
}

func TestStopReapsHiddenDescendantInDetachedGroup(t *testing.T) {
	hideChildEnvironments(t, false)
	for _, action := range []string{"Stop", "Close", "Cancel"} {
		t.Run(action, func(t *testing.T) {
			check := unmarkedBystanders(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, done, pid := hiddenLaunch(t, ctx, "sleep 60", false)
			switch action {
			case "Stop":
				p.Stop()
			case "Close":
				p.Close()
			case "Cancel":
				cancel()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not settle")
			}
			waitGone(t, pid)
			check()
			p.Close()
			check()
		})
	}
}

func TestCloseReapsHiddenDescendantAfterLeaderExit(t *testing.T) {
	// Hide the group member too, so only group discovery reaches this tree.
	hideChildEnvironments(t, true)
	check := unmarkedBystanders(t)
	p, done, pid := hiddenLaunch(t, context.Background(), "exit 0", false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !alive(pid) {
		t.Fatal("child exited before Close")
	}
	p.Close()
	waitGone(t, pid)
	check()
}

func TestRunReapsHiddenDescendantWhenLeaderKilled(t *testing.T) {
	hideChildEnvironments(t, false)
	check := unmarkedBystanders(t)
	p, done, pid := hiddenLaunch(t, context.Background(), "kill -KILL $$", false)
	if err := <-done; err == nil {
		t.Fatal("leader was not killed")
	}
	waitGone(t, pid)
	check()
	p.Close()
	check()
}

func TestStopReapsHiddenPlatformDescendant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS platform environment check")
	}
	check := unmarkedBystanders(t)
	p, done, pid := hiddenLaunch(t, context.Background(), "sleep 60", true)
	if len(environment(pid)) != 0 {
		t.Skip("OS exposes platform binary environment")
	}
	p.Stop()
	<-done
	waitGone(t, pid)
	p.Close()
	check()
}

func TestKillTreeRefusesUnknownOrReusedIdentity(t *testing.T) {
	snapshot := map[int]string{10: "", 11: "old", 12: "same"}
	var signalled []int
	killTreeWith(snapshot, func(pid int) string {
		if pid == 11 {
			return "new"
		}
		return "same"
	}, func(pid int) { signalled = append(signalled, pid) })
	if len(signalled) != 1 || signalled[0] != 12 {
		t.Fatalf("signalled %v", signalled)
	}
}

func TestTreeRetainsOnlySameBirthRoots(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	cmd := exec.Command("/bin/sleep", "60")
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	birth := processIdentity(pid)
	if birth == "" {
		t.Fatal("no birth identity")
	}
	if got := tree(newLaunchToken(), time.Now(), 0, map[int]string{pid: birth}); got[pid] != birth {
		t.Fatalf("lost remembered hidden root: %v", got)
	}
	for _, identity := range []string{"", birth + "-reused"} {
		if got := tree(newLaunchToken(), time.Now(), 0, map[int]string{pid: identity}); got[pid] != "" {
			t.Fatalf("admitted unknown or reused root: %v", got)
		}
	}
}

func TestTreeSnapshotsHiddenDetachedDescendants(t *testing.T) {
	procs := []candidate{
		{pid: 100, group: 100, identity: "leader"},
		{pid: 101, parent: 100, group: 100, identity: "marked"},
		{pid: 102, parent: 101, group: 102, identity: "hidden"},
		{pid: 103, parent: 102, group: 103, identity: "hidden-child"},
		{pid: 104, parent: os.Getpid(), group: 104, identity: "bystander"},
		{pid: 105, parent: os.Getpid(), group: 0, identity: "bystander-child"},
	}
	readEnv := func(pid int) []string {
		if pid == 101 {
			return []string{launchVariable + "=token"}
		}
		return nil
	}
	for _, group := range []int{0, 100} {
		snapshot := treeFrom(procs, "token", group, nil, readEnv)
		for _, pid := range []int{101, 102, 103} {
			if snapshot[pid] == "" {
				t.Errorf("group %d lost descendant %d", group, pid)
			}
		}
		for _, pid := range []int{104, 105} {
			if snapshot[pid] != "" {
				t.Errorf("selected bystander %d", pid)
			}
		}
		var signalled []int
		killTreeWith(snapshot, func(pid int) string {
			for _, proc := range procs {
				if proc.pid == pid {
					return proc.identity
				}
			}
			return ""
		}, func(pid int) { signalled = append(signalled, pid) })
		if len(signalled) != len(snapshot) {
			t.Fatal("snapshot was not signalled")
		}
	}
	// Once the leader exits, a hidden group member must still root its detached
	// descendants. Without group roots, the marker sweep cannot see any of them.
	hidden := func(int) []string { return nil }
	if got := treeFrom(procs[1:], "token", 0, nil, hidden); len(got) != 0 {
		t.Fatalf("marker sweep selected hidden processes: %v", got)
	}
	got := treeFrom(procs[1:], "token", 100, nil, hidden)
	if len(got) != 3 || got[101] != "marked" || got[102] != "hidden" || got[103] != "hidden-child" {
		t.Fatalf("group root lost hidden descendants or selected bystanders: %v", got)
	}
	// After the marked ancestor disappears, a remembered hidden member remains
	// a root for children forked later; reuse must revoke that ownership.
	remaining := procs[2:]
	for _, identity := range []string{"hidden", "reused", ""} {
		snapshot := treeFrom(remaining, "token", 0, map[int]string{102: identity}, readEnv)
		if (snapshot[103] != "") != (identity == "hidden") {
			t.Fatalf("remembered %q selected %v", identity, snapshot)
		}
	}
}
