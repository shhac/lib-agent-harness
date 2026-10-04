package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPortClientMissingDeveloperToolsNeverInvokesPython(t *testing.T) {
	missing := errors.New("synthetic missing developer tools")
	invoked := false
	err := checkPortClient(t.Context(), func(context.Context) error { return missing }, func(context.Context) error { invoked = true; return nil })
	if err != missing || invoked {
		t.Fatal("Python stub invoked without tools")
	}
}

func TestPortClientBindAndListenFailuresReportImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// These synthetic socket failures exercise the helper's status protocol,
	// outside Seatbelt. Real proof tests separately require Apple's interpreter
	// under the actual profile; a PATH interpreter cannot prove that boundary.
	interpreter, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("test interpreter unavailable", err)
	}
	for _, phase := range []string{"bind", "listen"} {
		for _, client := range []string{"attempt", "inbound"} {
			t.Run(client+"/"+phase, func(t *testing.T) {
				// Socket primitives are fake: this regression neither binds a real port
				// nor needs a nested sandbox. The production helper and status protocol
				// are identical; only the socket constructor is replaced here.
				fixture := "import socket,errno\nclass Fake:\n def setsockopt(self,*a): pass\n def settimeout(self,*a): pass\n def close(self): pass\n def bind(self,*a): " + map[string]string{"bind": "raise OSError(errno.EPERM,'fixture')", "listen": "pass"}[phase] + "\n def listen(self,*a): raise OSError(errno.EPERM,'fixture')\nsocket.socket=lambda *a: Fake()\n"
				attempts, _ := json.Marshal([]portAttempt{{"positive-bind", "127.0.0.1", 3000, "fixture"}})
				program, args, label := portClient, []string{string(attempts)}, "fixture"
				if client == "inbound" {
					program, args, label = inboundPortClient, []string{"3000", "fixture"}, "inbound"
				}
				step, stop := context.WithTimeout(ctx, 2*time.Second)
				defer stop()
				start := time.Now()
				output, err := exec.CommandContext(step, interpreter, append([]string{"-c", fixture + program}, args...)...).CombinedOutput()
				if step.Err() != nil {
					t.Fatal("failed bind/listen hung", step.Err())
				}
				if err == nil || !strings.Contains(string(output), "unavailable-"+label+"-"+phase+"-errno-1") || strings.Contains(string(output), "ports-ran") {
					t.Fatalf("bad failure status: %v %s", err, output)
				}
				if time.Since(start) >= 2*time.Second {
					t.Fatal("failure did not settle immediately")
				}
			})
		}
	}
}
