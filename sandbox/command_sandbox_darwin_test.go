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
