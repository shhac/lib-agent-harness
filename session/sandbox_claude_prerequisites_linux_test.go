//go:build linux

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestClaudeMissingSocatRefusesAllPrelaunchPaths(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	binary, log := fakeHarness(t, fakeSandboxOK)
	raw := sandboxOptions(t, harness.Claude, binary, true)
	raw.Sandbox.Loopback = true
	raw.Provider.CLI.Home = t.TempDir()
	raw.RuntimeHome = filepath.Join(t.TempDir(), "must-not-create")
	o, err := normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	ref := reference(o, "unused")
	for _, call := range []func() error{
		func() error { return VerifySandbox(context.Background(), raw) },
		func() error {
			s, e := Start(context.Background(), raw)
			if s != nil {
				s.Close()
			}
			return e
		},
		func() error {
			s, e := Resume(context.Background(), raw, ref)
			if s != nil {
				s.Close()
			}
			return e
		},
	} {
		err := call()
		var capability *CapabilityError
		if !errors.As(err, &capability) || capability.Reason != ClaudeLinuxSandboxRequiresSocat || capability.Code != CapabilitySandboxUnavailable {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("CLI launched before prerequisite refusal")
	}
	if _, err := os.Stat(raw.RuntimeHome); !os.IsNotExist(err) {
		t.Fatal("runtime prepared before prerequisite refusal")
	}
	// Rechecking a cache hit also reports a removed prerequisite.
	key, err := sandboxKey(o, &launch{extra: sandboxArgs(o)})
	if err != nil {
		t.Fatal(err)
	}
	verified.recordLoopback(key, loopbackEvidence{scope: "per-command"})
	defer func() {
		verified.mu.Lock()
		delete(verified.seen, key)
		delete(verified.loopback, key)
		verified.mu.Unlock()
	}()
	if err := VerifySandbox(context.Background(), raw); err == nil {
		t.Fatal("cache bypassed missing socat")
	}
}
