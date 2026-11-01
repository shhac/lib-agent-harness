package session

import (
	"context"
	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"os"
	"testing"
)

func TestEdgeVerifyHasNoRealStateOrLease(t *testing.T) {
	testenv.RequireLoopback(t)
	oldProbe, oldPlan := edgeProbe, sandboxbridge.BuildEdge
	defer func() { edgeProbe = oldProbe; sandboxbridge.BuildEdge = oldPlan }()
	probes := 0
	edgeProbe = func(context.Context, Options) error { probes++; return nil }
	sandboxbridge.BuildEdge = func(sandboxbridge.EdgeConfig) (sandboxbridge.EdgePlan, error) {
		return sandboxbridge.EdgePlan{Key: t.Name()}, nil
	}
	binary, _ := fakeHarness(t, "edge-session")
	runtime, control := t.TempDir(), hostDir(t)
	if err := os.Chmod(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	o := Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: binary, Home: t.TempDir()}}, WorkDir: t.TempDir(), RuntimeHome: runtime, Sandbox: &Sandbox{Edge: &Edge{ClaudeToken: "dummy"}, Tools: &ToolHost{Server: "edge", Dir: control, Bridge: Bridge{Path: "/usr/bin/true"}, Handler: echoHandler(t), Tools: []ToolDefinition{{Name: "read_file", Schema: map[string]any{"type": "object"}}}}}}
	if err := VerifySandbox(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatal("proof not shared", probes)
	}
	entries, err := os.ReadDir(runtime)
	if err != nil || len(entries) != 0 {
		t.Fatal("Verify prepared real runtime", err)
	}
	lease, err := holdLease(leasePath(control))
	if err != nil {
		t.Fatal("Verify leaked lease", err)
	}
	lease.Close()
	if _, err = os.Stat(secretPath(control)); !os.IsNotExist(err) {
		t.Fatal("Verify opened real tool host", err)
	}
}
