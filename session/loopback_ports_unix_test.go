//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestSelectedPortsNeverInvokeFakeHarness(t *testing.T) {
	binary, log := fakeHarness(t, fakeClean)
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok, harness.CommandCode} {
		root := t.TempDir()
		o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: filepath.Join(root, "login")}}, WorkDir: root, RuntimeHome: filepath.Join(root, "runtime"), Sandbox: &Sandbox{Loopback: true, LoopbackPorts: []int{8080}}}
		_, err := Start(context.Background(), o)
		requireSelectedPortsRefusal(t, err, selectedPortsCode())
		_, err = Resume(context.Background(), o, Ref{})
		requireSelectedPortsRefusal(t, err, selectedPortsCode())
		err = VerifySandbox(context.Background(), o)
		requireSelectedPortsRefusal(t, err, selectedPortsCode())
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fake harness was invoked: %v", err)
	}
}
