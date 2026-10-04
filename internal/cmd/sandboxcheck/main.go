// sandboxcheck runs the library's own check inside its proved command sandbox.
// Run it from an unsandboxed repository checkout with dependencies cached.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shhac/lib-agent-harness/sandbox"
)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func checkCommand(goBinary string, noSkip bool) string {
	prefix := `export GOCACHE="$TMPDIR/go-cache"; `
	if noSkip {
		prefix += "export AGENT_HARNESS_TEST_NO_SKIP=1; "
	}
	return prefix + quote(goBinary) + " vet ./... && " + quote(goBinary) + " test -race ./..."
}

func run(ctx context.Context, noSkip bool) (int, error) {
	root, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return 1, fmt.Errorf("run sandboxcheck from the module root: %w", err)
	}
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.CommandContext(ctx, goBinary, "env", "GOMODCACHE")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off")
	cache, err := cmd.Output()
	if err != nil {
		return 1, fmt.Errorf("locating the cached modules: %w", err)
	}
	modCache := strings.TrimSpace(string(cache))
	home, err := os.MkdirTemp("", "sandboxcheck-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(home)
	s, err := sandbox.Open(ctx, sandbox.Options{
		WorkDir: root, RuntimeHome: home, Write: true,
		Read: []string{runtime.GOROOT(), modCache}, Timeout: 10 * time.Minute,
		Env: []string{"GOMODCACHE=" + modCache, "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off"},
	})
	if err != nil {
		return 1, err
	}
	r, runErr := s.Run(ctx, sandbox.CommandRequest{Command: checkCommand(goBinary, noSkip)})
	closeErr := s.Close()
	fmt.Print(r.Stdout)
	fmt.Fprint(os.Stderr, r.Stderr)
	if runErr != nil {
		return 1, runErr
	}
	if closeErr != nil {
		return 1, closeErr
	}
	if r.TimedOut || r.Truncated {
		return 1, fmt.Errorf("check incomplete: timed_out=%v truncated=%v", r.TimedOut, r.Truncated)
	}
	return r.ExitCode, nil
}

func main() {
	noSkip := flag.Bool("no-skip", false, "fail on refused test prerequisites inside the sandbox")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := run(ctx, *noSkip)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
