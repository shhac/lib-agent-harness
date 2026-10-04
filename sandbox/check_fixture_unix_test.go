//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// Re-exec only: an ordinary suite run does not create a sandbox recursively.
func TestCommandSandboxFixture(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "nested":
		if runtime.GOOS == "darwin" {
			testenv.RequireNestedSandbox(t)
		} else {
			// Use the actual library trial, without the outer proof's network prerequisite.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			testenv.RequireBwrap(t, func() error {
				_, _, err := sandboxhook.CommandTrial(ctx, t.TempDir(), t.TempDir(), t.TempDir())
				var p *ProofError
				if errors.As(err, &p) && (p.Code == CapabilitySandboxToolMissing || p.Code == CapabilitySandboxToolOutdated || p.Code == CapabilitySandboxNamespacesUnavailable) {
					return errors.Join(fs.ErrPermission, err)
				}
				return err
			})
		}
	case "socket":
		testenv.RequireUnixSocket(t)
	case "loopback":
		testenv.RequireLoopback(t)
	default:
		t.Fatal("unknown fixture")
	}
}

func TestCommandSandboxRunSkipsRefusedCapabilities(t *testing.T) {
	requireCommandPlatform(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := commandSandboxOptions(t, false)
	// Pad the private scratch path beyond shortPrivateDir's limit on both OSes.
	opts.RuntimeHome = filepath.Join(opts.RuntimeHome, strings.Repeat("p", 100))
	if err := os.Mkdir(opts.RuntimeHome, 0700); err != nil {
		t.Fatal(err)
	}
	opts.Read = []string{filepath.Dir(binary)}
	opts.Timeout = time.Minute
	s := openTestCommandSandbox(t, opts)
	modes := []string{"nested", "socket"}
	// Bubblewrap isolates host networking but allows the namespace's own localhost.
	// Seatbelt without Loopback also refuses binds, so verify that refusal on macOS.
	if runtime.GOOS == "darwin" {
		modes = append(modes, "loopback")
	}
	for _, mode := range modes {
		for _, strict := range []bool{false, true} {
			// Env refuses AGENT_HARNESS_* and does not inherit it, even in CI.
			command := workbenchShellQuote(binary) + " -test.v -test.run='^TestCommandSandboxFixture$' -- " + mode
			if strict {
				command = testenv.NoSkipVariable + "=1 " + command
			}
			r, err := s.Run(context.Background(), CommandRequest{Command: command})
			if err != nil || r.TimedOut || r.Truncated {
				t.Fatalf("%s strict=%v: %+v %v", mode, strict, r, err)
			}
			output := r.Stdout + r.Stderr
			capability := map[string]string{"nested": "sandbox", "socket": "90-byte", "loopback": "loopback listener"}[mode]
			if !strings.Contains(output, capability) {
				t.Fatalf("%s missing capability reason: %s", mode, output)
			}
			if strict {
				if r.ExitCode == 0 || !strings.Contains(output, "--- FAIL: TestCommandSandboxFixture") || !strings.Contains(output, "forbids skipping") {
					t.Fatalf("strict fixture passed or lost its reason: %+v", r)
				}
			} else if r.ExitCode != 0 || !strings.Contains(output, "--- SKIP: TestCommandSandboxFixture") || strings.Contains(output, "--- FAIL") {
				t.Fatalf("refused fixture failed instead of skipping: %+v", r)
			}
		}
	}
}
