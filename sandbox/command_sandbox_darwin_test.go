package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func requireCommandPlatform(t *testing.T) { t.Helper(); requireWorkbenchSeatbelt(t) }

func TestCommandSandboxInboundEvidence(t *testing.T) {
	good := "tmp\ntmpdir\nsystem\nreadset\nloopback\nbound\ninbound\ncanary-ran\n"
	if err := judgeWorkbench(good, false, true, true); err != nil {
		t.Fatal(err)
	}
	if judgeWorkbench(strings.ReplaceAll(good, "inbound\n", ""), false, true, true) == nil {
		t.Fatal("missing inbound accepted")
	}
}

func TestCommandSandboxStartedServerWithoutLoopback(t *testing.T) {
	testenv.RequireLoopback(t) // Provider fixtures and canaries need a real loopback bind.
	s := openTestCommandSandbox(t, commandSandboxOptions(t, false))
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Start(context.Background(), CommandRequest{Command: fmt.Sprintf("while true; do nc -l 127.0.0.1 %d; sleep 1; done", port)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("server reachable without Loopback")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.Stop()
}

// The /usr/bin shims resolve the owner's xcode-select choice through /var and,
// for a full Xcode, need its whole bundle and license acceptance.
func TestCommandSandboxRunsSelectedDeveloperTools(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	// A fresh sandbox's first developer tool waits seconds on xcodebuild's
	// first-launch check, longer under -race load.
	opts.Timeout = time.Minute
	s := openTestCommandSandbox(t, opts)
	r, err := s.Run(context.Background(), CommandRequest{Command: "printf 'int main(void){return 0;}' > a.c && /usr/bin/cc a.c -o a && ./a && /usr/bin/git --version >/dev/null && /usr/bin/make --version >/dev/null && echo built"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "built") {
		t.Fatalf("developer tools unusable: %+v", r)
	}
}

func TestCommandSandboxGitMetadataOutsideScratchIsProtected(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	opts.Timeout = time.Minute
	s := openTestCommandSandbox(t, opts)
	// A fresh sandbox's first developer-tool shim waits on xcodebuild's
	// first-launch check, longer under -race load. Warm it outside the measured runs.
	r, err := s.Run(context.Background(), CommandRequest{Command: "/usr/bin/git --version", Timeout: time.Minute})
	if err != nil || r.TimedOut || r.ExitCode != 0 {
		t.Fatalf("git warm-up failed: err=%v result=%+v", err, r)
	}
	run := func(command string) CommandResult {
		t.Helper()
		r, err := s.Run(context.Background(), CommandRequest{Command: command, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if r.TimedOut {
			t.Fatalf("measured command %q timed out: %+v", command, r)
		}
		return r
	}
	if r := run(`git init -q "$TMPDIR/fixture" && git -C "$TMPDIR/fixture" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m x && echo ok > "$TMPDIR/fixture/.git/probe"`); r.ExitCode != 0 {
		t.Fatalf("private git repository unusable: %+v", r)
	}
	if r := run("echo x > .git/probe"); r.ExitCode == 0 {
		t.Fatalf("workspace git write succeeded: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(opts.WorkDir, ".git", "probe")); !os.IsNotExist(err) {
		t.Fatalf("workspace git probe exists: %v", err)
	}
	if r := run("mkdir nested && git init -q nested"); r.ExitCode == 0 {
		t.Fatalf("workspace nested git init succeeded: %+v", r)
	}
}

func TestDeveloperBundle(t *testing.T) {
	for dir, want := range map[string]string{
		"/Applications/Xcode.app/Contents/Developer":      "/Applications/Xcode.app",
		"/Applications/Xcode-beta.app/Contents/Developer": "/Applications/Xcode-beta.app",
		"/Library/Developer/CommandLineTools":             "/Library/Developer/CommandLineTools",
	} {
		if got := developerBundle(dir); got != want {
			t.Errorf("developerBundle(%q) = %q, want %q", dir, got, want)
		}
	}
}

// Node reads uname(3) as it loads its os module and aborts when refused.
func TestCommandSandboxAnswersUname(t *testing.T) {
	s := openTestCommandSandbox(t, commandSandboxOptions(t, false))
	r, err := s.Run(context.Background(), CommandRequest{Command: "uname -a >/dev/null && echo named"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "named") {
		t.Fatalf("uname refused: %+v", r)
	}
}
