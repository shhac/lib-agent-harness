//go:build darwin || linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
