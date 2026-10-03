package sandbox

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
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
	s := openTestCommandSandbox(t, commandSandboxOptions(t, false))
	r, err := s.Run(context.Background(), CommandRequest{Command: "printf 'int main(void){return 0;}' > a.c && /usr/bin/cc a.c -o a && ./a && /usr/bin/git --version >/dev/null && /usr/bin/make --version >/dev/null && echo built"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "built") {
		t.Fatalf("developer tools unusable: %+v", r)
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
