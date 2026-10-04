package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// A missing binary and absent login must not displace the network refusal.
// Neither start nor resume may prepare a runtime or publish proof for it.
func TestCodexLoopbackRefusesBeforeLaunch(t *testing.T) {
	for _, operation := range []string{"start", "resume", "verify"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			o := Options{
				Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{
					Binary: filepath.Join(root, "must-not-launch"), Home: filepath.Join(root, "login"),
				}},
				WorkDir: root, RuntimeHome: filepath.Join(root, "runtime"),
				Sandbox: &Sandbox{Write: true, Loopback: true},
			}
			var err error
			switch operation {
			case "start":
				_, err = Start(context.Background(), o)
			case "resume":
				// Even an incompatible reference must not start network work.
				_, err = Resume(context.Background(), o, Ref{})
			case "verify":
				err = VerifySandbox(context.Background(), o)
			}
			if runtime.GOOS == "windows" {
				var capability *CapabilityError
				if !errors.As(err, &capability) || capability.Code != CapabilitySandboxUnavailable {
					t.Fatalf("Windows pre-launch refusal: %v", err)
				}
			} else {
				var refused *UnsupportedError
				if !errors.As(err, &refused) || refused.Code != RefusedNotOffered || refused.Operation != "loopback" {
					t.Fatalf("network pre-launch refusal: %v", err)
				}
				if refused.Capability != harness.Support(harness.Codex, harness.Session, harness.Loopback) {
					t.Fatalf("displaced capability: %+v", refused)
				}
				for _, observation := range []string{"owner-observed macOS", "closed network refused loopback", "off-machine TCP 443", "TCP/UDP port 53", "native session"} {
					if !strings.Contains(refused.Capability.Reason, observation) {
						t.Errorf("%s refusal omits %q: %+v", operation, observation, refused)
					}
				}
			}
			for _, path := range []string{o.Provider.CLI.Home, o.RuntimeHome} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused launch touched private state: %v", err)
				}
			}
		})
	}
}
