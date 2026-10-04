//go:build darwin || linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestInterfaceClientProtocol(t *testing.T) {
	for _, attempts := range [][]interfaceAttempt{nil, interfaceAttempts([]string{"127.0.0.1"}, false, true)} {
		t.Run(fmt.Sprint(len(attempts)), func(t *testing.T) {
			if len(attempts) > 0 {
				testenv.RequireLoopback(t)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, err := exec.CommandContext(ctx, "/bin/sh", "-c", interfaceCanary("/usr/bin/python3", attempts)).CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("attempt client: %v: %s", err, out)
			}
			observed, err := judgeInterfaceAttempts(string(out), attempts, interfaceAllLocal)
			if err != nil {
				t.Fatalf("client protocol: %v: %s", err, out)
			}
			for _, o := range observed {
				if o.Errno != 0 {
					t.Fatalf("positive control: %+v", o)
				}
			}
		})
	}
}
