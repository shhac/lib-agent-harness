//go:build !windows

package session

import (
	"context"
	"runtime"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestClaudeLoopbackOfferedAfterBaseProof(t *testing.T) {
	testenv.RequireUnixSocket(t)
	testenv.RequireProcessGroup(t)
	if support := harness.Support(harness.Claude, harness.Session, harness.Loopback); support.Availability != harness.Unknown {
		t.Fatalf("not offered: %+v", support)
	}
	original := engines[harness.Claude].sandbox.probe
	defer func() { engines[harness.Claude].sandbox.probe = original }()
	count := 0
	engines[harness.Claude].sandbox.probe = func(ctx context.Context, o Options, l *launch) (loopbackEvidence, error) {
		count++
		if !o.Sandbox.Loopback {
			t.Error("request dropped Loopback")
		}
		// A passing base proof with explicitly unavailable diagnostics.
		evidence := loopbackEvidence{interfaces: []ncInterfaceObservation{{"0.0.0.0", "bind", "unavailable"}}}
		if runtime.GOOS == "linux" {
			evidence.scope = "per-command"
		}
		return evidence, nil
	}
	binary, _ := fakeHarness(t, fakeSandboxOK)
	raw := sandboxOptions(t, harness.Claude, binary, true)
	raw.Provider.CLI.Home = t.TempDir()
	raw.Sandbox.Loopback = true
	ctx := context.Background()
	if err := VerifySandbox(ctx, raw); err != nil {
		t.Fatal(err)
	}
	key, err := realClaudeProofKey(raw)
	if err != nil || !verified.holds(key) {
		t.Fatalf("verification absent: %v", err)
	}
	if runtime.GOOS == "linux" {
		verified.mu.Lock()
		scope := verified.loopback[key].scope
		verified.mu.Unlock()
		if scope != "per-command" {
			t.Fatal("Linux scope not recorded")
		}
	}
	s, err := Start(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	if _, err := s.Release(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Resume(ctx, raw, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("base proof not reused for identical launch: %d", count)
	}
}

func TestClaudeLoopbackOnlyStrictRequestRefusedOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		return
	}
	raw := Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: t.TempDir(), Sandbox: &Sandbox{Loopback: true}}
	if _, err := normalize(raw); err != nil {
		t.Fatalf("Loopback refused: %v", err)
	}
	raw.Sandbox.LoopbackLocalOnly = true
	if _, err := normalize(raw); err == nil {
		t.Fatal("strict local-only request admitted")
	}
}
