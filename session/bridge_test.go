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

// holdLockEnv makes the test binary re-execute itself as a stand-in for a
// bridge running under a harness: it takes the lock and waits to be killed.
const holdLockEnv = "AGENT_HARNESS_TEST_HOLD_LOCK"

func TestMain(m *testing.M) {
	if path := os.Getenv(holdLockEnv); path != "" {
		lock, err := holdBridgeLock(path)
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
	// The lock is held for the bridge's whole life, which is what makes an
	// orphaned harness discoverable after a crash.
	held, err := readBridgeLock(filepath.Join(h.cfg.Dir, "bridge.lock"))
	if err != nil || held == nil {
		t.Fatalf("a running bridge did not hold its lock: %v %v", held, err)
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

func TestReclaimReportsNothingWhenNoSubtreeSurvived(t *testing.T) {
	dir := privateDir(t)
	out, err := Reclaim(context.Background(), dir)
	if err != nil || out.Found || out.Reclaimed {
		t.Fatalf("an absent lock was not reported as nothing to reclaim: %+v %v", out, err)
	}
	// A lock file that exists but is free is equally positive evidence.
	if err = os.WriteFile(filepath.Join(dir, "bridge.lock"), []byte(`{"group":424242}`), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err = Reclaim(context.Background(), dir); err != nil || out.Found {
		t.Fatalf("a free lock was treated as a surviving subtree: %+v %v", out, err)
	}
}

// Stopping whatever a session operated on does not establish that the harness
// stopped. Reclaim ends the surviving group and confirms it is gone.
func TestReclaimTerminatesAnOrphanedSubtree(t *testing.T) {
	dir := privateDir(t)
	lock := filepath.Join(dir, "bridge.lock")
	cmd := exec.Command(os.Args[0], "-test.run=TestReclaimTerminatesAnOrphanedSubtree")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+lock)
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
	held, err := readBridgeLock(lock)
	if err != nil || held == nil || held.Group != cmd.Process.Pid {
		t.Fatalf("lock does not name the live holder's group: %+v %v", held, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := Reclaim(ctx, dir)
	if err != nil {
		t.Fatalf("reclaim failed: %v", err)
	}
	if !out.Found || !out.Reclaimed || out.Group != cmd.Process.Pid {
		t.Fatalf("orphan was not reclaimed: %+v", out)
	}
	if held, err = readBridgeLock(lock); err != nil || held != nil {
		t.Fatalf("lock still held after reclamation: %+v %v", held, err)
	}
}

// Recovery must never signal the group it is running in, and must never treat
// an unusable record as a licence to kill something.
func TestReclaimRefusesToSignalItsOwnGroup(t *testing.T) {
	dir := privateDir(t)
	lock := filepath.Join(dir, "bridge.lock")
	held, err := holdBridgeLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	out, err := Reclaim(context.Background(), dir)
	if !errors.Is(err, ErrUnreclaimed) {
		t.Fatalf("reclaiming this process's own group was not refused: %+v %v", out, err)
	}
	if !out.Found {
		t.Error("a held lock was not reported as a surviving subtree")
	}
	if terminateGroup(0) == nil || terminateGroup(1) == nil {
		t.Error("an implausible process group was accepted for termination")
	}
}
