package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// A command may use Unix-domain sockets it makes in its own scratch or
// workspace, as a daemon under test does, and still reaches no other socket.
func TestCommandsUseTheirOwnUnixSocketsAndNoOthers(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	// Socket paths are limited to about a hundred bytes, which a test's own
	// temporary directory can exceed.
	root, err := os.MkdirTemp("/private/tmp", "lah-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	l := workbenchLayout{Work: filepath.Join(root, "w"), Home: filepath.Join(root, "h"), Tmp: filepath.Join(root, "t"), System: workbenchSystemDirs(), Write: true}
	outside := filepath.Join(root, "o")
	for _, dir := range []string{l.Work, l.Home, l.Tmp, outside} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", filepath.Join(outside, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("outside-reached\n"))
			c.Close()
		}
	}()
	q := sandboxprobe.ShellQuote
	script := ""
	for _, dir := range []string{l.Tmp, l.Work} {
		s := q(filepath.Join(dir, "s"))
		script += "/usr/bin/nc -U -l " + s + " >/dev/null & sleep 0.3; echo hi | /usr/bin/nc -U -w 2 " + s + " && echo own-ok; wait\n"
	}
	script += "/usr/bin/nc -U -w 2 " + q(filepath.Join(outside, "s")) + " </dev/null || echo outside-refused\n"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", seatbeltProfile(l), "/bin/sh", "-c", script)
	cmd.Dir = l.Work
	cmd.Env = workbenchProbeEnvironment(l)
	output, _ := cmd.CombinedOutput()
	got := string(output)
	if strings.Count(got, "own-ok") != 2 {
		t.Fatalf("a command could not use a socket in its own scratch and workspace: %s", got)
	}
	if strings.Contains(got, "outside-reached") || !strings.Contains(got, "outside-refused") {
		t.Fatalf("a command reached a socket outside what it may write: %s", got)
	}
}
