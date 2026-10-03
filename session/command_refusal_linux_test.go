package session

import (
	"errors"
	"github.com/shhac/lib-agent-harness/sandbox"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

func TestWorkbenchLinuxCommandRefusalOrder(t *testing.T) {
	for _, kind := range []string{"timeout", "read", "loopback-before-placement", "loopback-before-env"} {
		t.Run(kind, func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			o.Workbench.Commands = &Commands{Loopback: true}
			want := RefusedNotOffered
			switch kind {
			case "timeout":
				o.Workbench.Commands.Timeout = 11 * time.Minute
				want = RefusedLimit
			case "read":
				o.Workbench.Commands.Read = []string{"relative"}
				want = RefusedSandboxRead
			case "loopback-before-placement":
				o.WorkDir = "/usr"
			case "loopback-before-env":
				o.Workbench.Commands.Env = []string{"HOME=/tmp"}
			}
			// This helper is called after workspace normalization. Its established
			// prefix validates timeout/home/read before refusing hosted loopback.
			_, err := sandboxbridge.NormalizeWorkbench(commandOptions(o))
			var raw *sandbox.RefusalError
			if !errors.As(err, &raw) || raw.Code != want {
				t.Fatal(err)
			}
			err = fromSandbox(err, o)
			var sessionErr *UnsupportedError
			if !errors.As(err, &sessionErr) || sessionErr.Code != want {
				t.Fatal(err)
			}
		})
	}
}
