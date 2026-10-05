//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStandaloneRunnerTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, 11 * time.Minute, 45 * time.Minute, MaxStandaloneTimeout} {
		t.Run(timeout.String(), func(t *testing.T) {
			o := commandSandboxOptions(t, false)
			o.Timeout = timeout
			proof, err := proveOptions(context.Background(), o, func(_ context.Context, n Options) (Proof, error) {
				return Proof{system: n.system, options: n, binary: "unlaunched-fixture"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(proof.options.RuntimeHome, "runner")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			r, err := NewRunner(o, proof, dir, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.Close(); err != nil {
					requireCommandCode(t, err, "command_cleanup_unknown")
				}
			})
			want := timeout
			if want == 0 {
				want = 2 * time.Minute
			}
			if r.Timeout() != want {
				t.Fatalf("timeout %v, want %v", r.Timeout(), want)
			}
		})
	}
}

func TestOpenedSandboxLongRequestTimeout(t *testing.T) {
	o := commandSandboxOptions(t, false)
	o.Timeout = 45 * time.Minute
	s, err := openWithProof(context.Background(), o, func(_ context.Context, n Options) (Proof, error) {
		return Proof{system: n.system, options: n, binary: "unlaunched-fixture"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			requireCommandCode(t, err, "command_cleanup_unknown")
		}
	})
	s.commands.execute = func(_ context.Context, _, _ string, timeout time.Duration, onStart func()) (CommandResult, error) {
		notifyCommandLaunch(onStart)
		return CommandResult{Stdout: timeout.String()}, nil
	}
	got, err := s.Run(context.Background(), CommandRequest{Command: "fixture", Timeout: 30 * time.Minute})
	if err != nil || got.Stdout != (30*time.Minute).String() {
		t.Fatalf("%+v %v", got, err)
	}
	_, err = s.Run(context.Background(), CommandRequest{Command: "fixture", Timeout: 46 * time.Minute})
	requireCommandCode(t, err, ArgumentsInvalid)
}

func TestRunnerFreezesSelectedPorts(t *testing.T) {
	o := Options{Loopback: true, LoopbackPorts: []int{3000}}
	frozen := o
	frozen.LoopbackPorts = []int{3000}
	p := (Proof{system: []string{"/system"}, options: frozen}).withRequest(o)
	o.LoopbackPorts[0] = 8080
	_, err := NewRunner(o, p, t.TempDir(), 0)
	var failure *StateError
	if !errors.As(err, &failure) || failure.Code != StateUnusable || p.request.LoopbackPorts[0] != 3000 {
		t.Fatal("mutated ports admitted", err)
	}
}

func TestRunnerRequiresMatchingProofBeforeState(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	o, err := normalize(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(o.RuntimeHome, "sessions", "legacy")
	if err = os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	proof := Proof{system: workbenchSystemDirs(), binary: "unlaunched-fixture", identity: "fixture", options: o}
	for _, kind := range []string{"zero", "mismatch", "budget", "outside"} {
		t.Run(kind, func(t *testing.T) {
			p := proof
			n := o
			dir := state
			budget := MinResult
			switch kind {
			case "zero":
				p = Proof{}
			case "mismatch":
				n.Write = !n.Write
			case "budget":
				budget = 1
			case "outside":
				dir = t.TempDir()
			}
			r, err := NewRunner(n, p, dir, budget)
			if r != nil || err == nil {
				if r != nil {
					r.Close()
				}
				t.Fatal("unproved runner admitted")
			}
			if kind == "budget" {
				var refused *RefusalError
				if !errors.As(err, &refused) || refused.Code != RefusedLimit {
					t.Fatal(err)
				}
			} else {
				var state *StateError
				if !errors.As(err, &state) || state.Code != StateUnusable {
					t.Fatal(err)
				}
			}
			if _, err = os.Stat(filepath.Join(state, "workbench-token.json")); !os.IsNotExist(err) {
				t.Fatal("failure wrote recovery state")
			}
		})
	}
	// Returning the system set cannot let a caller mutate its evidence.
	dirs := proof.SystemDirs()
	dirs[0] = "/changed"
	if proof.SystemDirs()[0] == "/changed" {
		t.Fatal("proof leaked its system slice")
	}
}

func TestRunnerUsesFrozenOptionsWithoutRenormalizing(t *testing.T) {
	// These synthetic options deliberately cannot be normalized anymore. A
	// matching proof must reach budget validation without path/system discovery.
	raw := Options{WorkDir: "/removed-work", RuntimeHome: "/removed-runtime", Read: []string{"/read"}, Env: []string{"LANG=C"}}
	normalized := raw
	normalized.Timeout = time.Minute
	normalized.system = []string{"/proved-system"}
	proof := Proof{options: normalized, system: normalized.system}.withRequest(raw)
	for _, options := range []Options{raw, normalized} {
		_, err := NewRunner(options, proof, "/unused", 1)
		var refused *RefusalError
		if !errors.As(err, &refused) || refused.Code != RefusedLimit {
			t.Fatalf("matching proof repeated normalization: %v", err)
		}
	}
	raw.Read[0] = "/widened"
	raw.Env[0] = "LANG=changed"
	_, err := NewRunner(raw, proof, "/unused", 1)
	var state *StateError
	if !errors.As(err, &state) || state.Code != StateUnusable {
		t.Fatalf("mutating the request changed its frozen evidence: %v", err)
	}
}
