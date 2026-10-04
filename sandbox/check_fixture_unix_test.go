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
			testenv.RequireBwrap(t, ctx, func() error {
				_, _, err := sandboxhook.CommandTrial(ctx, t.TempDir(), t.TempDir(), t.TempDir())
				if ctx.Err() != nil {
					return ctx.Err()
				}
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
			} else {
				command = testenv.NoSkipVariable + "=0 " + command
			}
			r, err := s.Run(context.Background(), CommandRequest{Command: command})
			if err != nil || r.TimedOut || r.Truncated {
				t.Fatalf("%s strict=%v: %+v %v", mode, strict, r, err)
			}
			output := r.Stdout + r.Stderr
			if !fixtureRefusalReason(output, runtime.GOOS, mode) {
				t.Fatalf("%s missing capability reason: %s", mode, output)
			}
			if strict {
				if r.ExitCode == 0 || !strings.Contains(output, "--- FAIL: TestCommandSandboxFixture") || !strings.Contains(output, "forbids skipping") {
					t.Fatalf("strict fixture passed or lost its reason: %+v", r)
				}
			} else if r.ExitCode != 0 || !strings.Contains(output, "--- SKIP: TestCommandSandboxFixture") || strings.Contains(output, "--- FAIL") {
				t.Fatalf("refused fixture failed instead of skipping: %+v", r)
			}
			t.Logf("child %s strict=%v exit=%d verified", mode, strict, r.ExitCode)
		}
	}
}

func fixtureRefusalReason(output, platform, mode string) bool {
	capability := map[string]string{"nested": "a nested OS sandbox", "socket": "a Unix domain socket under TMPDIR", "loopback": "a loopback listener"}[mode]
	if mode == "nested" && platform == "linux" {
		capability = "bubblewrap 0.8.0+ and unprivileged namespaces"
	}
	if capability == "" {
		return false
	}
	prefix := "environment refuses " + capability + ": "
	for _, line := range strings.Split(output, "\n") {
		_, suffix, ok := strings.Cut(line, prefix)
		suffix, _, _ = strings.Cut(suffix, " ("+testenv.NoSkipVariable+"=1 forbids skipping)")
		if ok && strings.TrimSpace(suffix) != "" && (mode != "socket" || strings.Contains(suffix, "90-byte")) {
			return true
		}
	}
	return false
}

func TestFixtureRefusalReason(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		capability := "a nested OS sandbox"
		if platform == "linux" {
			capability = "bubblewrap 0.8.0+ and unprivileged namespaces"
		}
		prefix := "environment refuses " + capability + ": "
		for _, suffix := range []string{"operation not permitted", "permission denied: namespace trial refused", "permission denied namespace trial: namespaces unavailable"} {
			for _, strict := range []bool{false, true} {
				output := "=== RUN   TestCommandSandboxFixture\n    fixture.go:1: " + prefix + suffix
				if strict {
					output += " (" + testenv.NoSkipVariable + "=1 forbids skipping)\n--- FAIL: TestCommandSandboxFixture (0.00s)\nFAIL\n"
				} else {
					output += "\n--- SKIP: TestCommandSandboxFixture (0.00s)\nPASS\n"
				}
				if !fixtureRefusalReason(output, platform, "nested") {
					t.Fatalf("complete strict=%v reason rejected: %s", strict, output)
				}
			}
		}
		for _, output := range []string{prefix, prefix + "  ", prefix + " (" + testenv.NoSkipVariable + "=1 forbids skipping)", "environment refuses bubblewrap 0.8.0+ and unprivileged names…", "operation not permitted"} {
			if fixtureRefusalReason(output, platform, "nested") {
				t.Fatalf("incomplete reason accepted: %q", output)
			}
		}
		wrong := "environment refuses a nested OS sandbox: sandbox_apply: operation not permitted"
		if platform == "darwin" {
			wrong = "environment refuses bubblewrap 0.8.0+ and unprivileged namespaces: permission denied"
		}
		for _, output := range []string{wrong, wrong + " (" + testenv.NoSkipVariable + "=1 forbids skipping)", "environment refuses a loopback listener: permission denied"} {
			if fixtureRefusalReason(output, platform, "nested") {
				t.Fatalf("wrong capability accepted: %s", output)
			}
		}
	}
	for _, platform := range []string{"linux", "darwin"} {
		if !fixtureRefusalReason("environment refuses a Unix domain socket under TMPDIR: socket path exceeds the library's 90-byte channel-directory limit", platform, "socket") {
			t.Fatal("socket limit reason rejected")
		}
		if fixtureRefusalReason("environment refuses a Unix domain socket under TMPDIR: unrelated failure", platform, "socket") {
			t.Fatal("lost socket limit evidence")
		}
		if !fixtureRefusalReason("environment refuses a loopback listener: listen: operation not permitted", platform, "loopback") {
			t.Fatal("loopback reason rejected")
		}
	}
}
