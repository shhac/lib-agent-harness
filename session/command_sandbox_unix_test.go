//go:build darwin || linux

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/process"
)

func commandSandboxOptions(t *testing.T, loopback bool) CommandSandboxOptions {
	t.Helper()
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	return CommandSandboxOptions{WorkDir: work, RuntimeHome: home, Write: true, Loopback: loopback, Timeout: 10 * time.Second, Env: []string{"PORT=1234"}}
}

func openTestCommandSandbox(t *testing.T, opts CommandSandboxOptions) *CommandSandbox {
	t.Helper()
	requireCommandPlatform(t)
	s, err := OpenCommandSandbox(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestCommandSandboxPreProofRefusals(t *testing.T) {
	for _, problem := range []string{"env", "read", "home"} {
		t.Run(problem, func(t *testing.T) {
			opts := commandSandboxOptions(t, false)
			switch problem {
			case "env":
				opts.Env = []string{"HOME=/tmp"}
			case "read":
				opts.Read = []string{"relative"}
			case "home":
				opts.WorkDir = "/"
			}
			_, err := OpenCommandSandbox(context.Background(), opts)
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("not a typed refusal: %v", err)
			}
			o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, WorkDir: opts.WorkDir, RuntimeHome: opts.RuntimeHome,
				Restriction: &Restriction{}, Workbench: &Workbench{Write: opts.Write, Commands: &Commands{Read: opts.Read, Env: opts.Env, Timeout: opts.Timeout}}}
			o, normalizeErr := normalizeRuntimeHome(o)
			if normalizeErr != nil {
				t.Fatal(normalizeErr)
			}
			_, normalizeErr = normalizeWorkbench(o)
			var hosted *UnsupportedError
			if !errors.As(normalizeErr, &hosted) || unsupported.Code != hosted.Code || unsupported.Operation != hosted.Operation {
				t.Fatalf("standalone/hosted refusal differs: %v / %v", err, normalizeErr)
			}
			if _, err := os.Stat(filepath.Join(opts.RuntimeHome, "commands")); !os.IsNotExist(err) {
				t.Fatal("refusal created command state")
			}
		})
	}
}

func TestCommandSandboxExpiredProofCreatesNoState(t *testing.T) {
	opts := commandSandboxOptions(t, true)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	count := workbenchCanaryRuns.Load()
	_, err := OpenCommandSandbox(ctx, opts)
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityProbeTimeout {
		t.Fatal(err)
	}
	if workbenchCanaryRuns.Load() != count {
		t.Fatal("expired proof reached canary")
	}
	if _, err := os.Stat(filepath.Join(opts.RuntimeHome, "commands")); !os.IsNotExist(err) {
		t.Fatal("expired proof created command state")
	}
}

func TestCommandSandboxRealRunAndClose(t *testing.T) {
	s := openTestCommandSandbox(t, commandSandboxOptions(t, false))
	token := commandSandboxTestToken(t, s)
	r, err := s.Run(context.Background(), CommandRequest{Command: `printf '%s' "$PORT"; printf err >&2; exit 7`})
	if err != nil || r.ExitCode != 7 || r.Stdout != "1234" || r.Stderr != "err" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = s.Run(context.Background(), CommandRequest{Command: "yes x | head -c 200000; yes y | head -c 200000 >&2"})
	if err != nil || !r.Truncated || len(r.Stdout) > skills.MaxOutputBytes+32 || len(r.Stderr) > skills.MaxOutputBytes+32 {
		t.Fatalf("bounded output: %+v %v", r, err)
	}
	r, err = s.Run(context.Background(), CommandRequest{Command: "sleep 30", Timeout: time.Second})
	if err != nil || !r.TimedOut || r.ExitCode != -1 {
		t.Fatalf("%+v %v", r, err)
	}
	assertCommandTreeGone(t, token)
	ctx, cancel := context.WithCancel(context.Background())
	h, err := s.Start(ctx, CommandRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := h.Result(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertCommandTreeGone(t, token)
	if _, err = s.Run(context.Background(), CommandRequest{Command: "sleep 30 >/dev/null 2>&1 &"}); err != nil {
		t.Fatal(err)
	}
	h, err = s.Start(context.Background(), CommandRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockSession(s.dir); err == nil {
		t.Fatal("lifetime lock not held")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	<-h.Done()
	assertCommandTreeGone(t, token)
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatal("state/scratch survived Close")
	}
}

func TestCommandSandboxRealLoopbackSuite(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			s := openTestCommandSandbox(t, commandSandboxOptions(t, allowed))
			port, err := freeLoopbackPort()
			if err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("nc -l 127.0.0.1 %d >/dev/null 2>&1 &\nlistener=$!\nsleep 1\nnc -z -w 2 127.0.0.1 %d\nresult=$?\nkill \"$listener\" 2>/dev/null; wait \"$listener\" 2>/dev/null\nexit \"$result\"", port, port)
			r, err := s.Run(context.Background(), CommandRequest{Command: script})
			if err != nil {
				t.Fatal(err)
			}
			wantSuccess := allowed || runtime.GOOS == "linux"
			if (r.ExitCode == 0) != wantSuccess {
				t.Fatalf("own localhost allowed=%t: %+v", allowed, r)
			}
			if runtime.GOOS == "linux" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				r, err = s.Run(context.Background(), CommandRequest{Command: fmt.Sprintf("nc -z -w 2 127.0.0.1 %d", listener.Addr().(*net.TCPAddr).Port)})
				if err != nil || r.ExitCode == 0 {
					t.Fatalf("host localhost reachable: %+v %v", r, err)
				}
			}
		})
	}
}

func TestCommandSandboxRealStartedServer(t *testing.T) {
	s := openTestCommandSandbox(t, commandSandboxOptions(t, true))
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Start(context.Background(), CommandRequest{Command: fmt.Sprintf("while true; do nc -l 127.0.0.1 %d; done", port)})
	if runtime.GOOS == "linux" {
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || unsupported.Operation != "start" {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sandbox server unreachable from host")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.Stop()
	select {
	case <-h.Done():
	default:
		t.Fatal("Stop did not settle")
	}
}

func TestCommandSandboxRealParallelClose(t *testing.T) {
	s := openTestCommandSandbox(t, commandSandboxOptions(t, false))
	token := commandSandboxTestToken(t, s)
	var wg sync.WaitGroup
	var markers []string
	for i := range 8 {
		marker := fmt.Sprintf("launched-%d", i)
		markers = append(markers, filepath.Join(sandboxhook.Access(s.ws.files).Root.Name(), marker))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Run(context.Background(), CommandRequest{Command: "printf ready > " + marker + "; exec sleep 30"})
			requireCommandCode(t, err, CommandSandboxClosed)
		}()
	}
	for _, marker := range markers {
		waitCommandMarker(t, marker)
	}
	if !process.TokenPresent(token.Token, token.Since) {
		t.Fatal("no live command witness before Close")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	assertCommandTreeGone(t, token)
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatal("state/scratch survived parallel Close")
	}
}

func commandSandboxTestToken(t *testing.T, s *CommandSandbox) workbenchToken {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "workbench-token.json"))
	if err != nil {
		t.Fatal(err)
	}
	var token workbenchToken
	if json.Unmarshal(data, &token) != nil || token.Token == "" || token.Since.IsZero() {
		t.Fatal("invalid token")
	}
	return token
}

func assertCommandTreeGone(t *testing.T, token workbenchToken) {
	t.Helper()
	if process.TokenPresent(token.Token, token.Since) {
		t.Fatal("command tree survived settlement")
	}
}

func waitCommandMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && string(data) == "ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("command never launched:", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandSandboxRealRunCancellationSettlesTree(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	s := openTestCommandSandbox(t, opts)
	token := commandSandboxTestToken(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := s.Run(ctx, CommandRequest{Command: "sleep 30 >/dev/null 2>&1 & printf ready > run-ready; wait"})
		result <- err
	}()
	waitCommandMarker(t, filepath.Join(opts.WorkDir, "run-ready"))
	if !process.TokenPresent(token.Token, token.Since) {
		t.Fatal("no live command witness before cancel")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertCommandTreeGone(t, token)
}

func TestCommandSandboxRecoveryAndLiveLock(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	s := openTestCommandSandbox(t, opts)
	h, err := s.Start(context.Background(), CommandRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	second := openTestCommandSandbox(t, opts)
	select {
	case <-h.Done():
		t.Fatal("live sandbox swept")
	default:
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	// Model launcher death by releasing only the lifetime lock. Its marked
	// supervisor/tree remains; next Open must discover and stop it.
	if err := s.lock.Close(); err != nil {
		t.Fatal(err)
	}
	s.lock = nil
	third := openTestCommandSandbox(t, opts)
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatal("stale directory retained")
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stale process survived recovery")
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandSandboxPlatformProofPolicy(t *testing.T) {
	opts := commandSandboxOptions(t, true)
	first := openTestCommandSandbox(t, opts)
	count := workbenchCanaryRuns.Load()
	second := openTestCommandSandbox(t, opts)
	want := count
	if runtime.GOOS == "linux" {
		want++
	}
	if workbenchCanaryRuns.Load() != want {
		t.Fatal("canary count does not match platform proof policy")
	}
	if first.dir == second.dir {
		t.Fatal("independent sandboxes share state")
	}
}

func TestLinuxOwnLoopbackEvidence(t *testing.T) {
	good := "tmp\ntmpdir\nsystem\nreadset\nprivilege-ok\nwitness-network-structural\nnetwork-structural-ok\nwitness-localhost-structural\nlocalhost-structural-ok\nwitness-socket-structural\nsocket-structural-ok\nown-loopback\ncanary-ran\n"
	if err := judgeLinuxWorkbench(good, false, false, false, true); err != nil {
		t.Fatal(err)
	}
	if judgeLinuxWorkbench(strings.ReplaceAll(good, "own-loopback\n", ""), false, false, false, true) == nil {
		t.Fatal("missing own-loopback accepted")
	}
}

func TestCommandSandboxPrivateLaunchObservation(t *testing.T) {
	count := 0
	started, code, known := readBwrapStatus(strings.NewReader("{\"child-pid\":1}\n{\"child-pid\":2}\n{\"exit-code\":7}\n"), func() { count++ })
	if !started || !known || code != 7 || count != 1 {
		t.Fatalf("%t %d %t launches=%d", started, code, known, count)
	}
	count = 0
	started, _, known = readBwrapStatus(strings.NewReader("{\"exit-code\":0}\n"), func() { count++ })
	if started || known || count != 0 {
		t.Fatal("exit without private launch evidence accepted")
	}
}
