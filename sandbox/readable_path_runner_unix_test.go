//go:build darwin || linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadablePathLowLevelRunner(t *testing.T) {
	requireCommandPlatform(t)
	o := commandSandboxOptions(t, false)
	hidden := t.TempDir()
	o.Env = []string{"PATH=" + hidden + ":/usr/bin:/bin"}
	p, err := Prove(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(p.options.RuntimeHome, "path-runner")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(o, p, state, MinResult)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := r.Execute(context.Background(), "printf ok; exit 9", ".", time.Second, nil)
	if err != nil || result.ExitCode != 9 || result.Stdout != "ok" || strings.Count(result.Stderr, "[harness PATH:") != 1 {
		t.Fatal(result, err)
	}
	t.Logf("low-level nonzero result: %+v", result)
	result, err = r.Execute(context.Background(), "sleep 30", ".", time.Millisecond*100, nil)
	if err != nil || !result.TimedOut || strings.Count(result.Stderr, "[harness PATH:") != 1 {
		t.Fatal(result, err)
	}
	t.Logf("low-level timeout result: %+v", result)
}

func TestClosedRunnerDoesNotPreparePath(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := "/bin/sh"
	identity, err := workbenchBinaryFingerprintForTest(binary)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise preparation/lifecycle directly with synthetic proof evidence;
	// no platform sandbox or model is launched by this regression.
	r, err := newCommandSandbox(commandConfig{options: Options{WorkDir: work, RuntimeHome: state, Timeout: time.Second}, proof: Proof{system: workbenchSystemDirs(), binary: binary, identity: identity}, stateDir: state, standalone: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		// Admission closes even when process inspection cannot confirm cleanup.
		// Preserve that uncertainty; this test makes no reaping-success claim.
		requireCommandCode(t, err, CommandCleanupUnknown)
	}
	result, err := r.Execute(context.Background(), "echo unsafe", ".", time.Second, nil)
	requireCommandCode(t, err, CommandSandboxClosed)
	if result != (CommandResult{}) {
		t.Fatal("closed runner prepared diagnostics", result)
	}
}
