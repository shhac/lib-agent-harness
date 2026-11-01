package session

import (
	"context"
	"errors"
	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CLI-independent evidence: the exact production edge and boundary script.
// NO_SKIP runners fail on unavailable controls instead of treating them as denial.
func TestEdgeRealBoundaryScript(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	testenv.RequireLoopback(t)
	// A hosted runner has no usable off-machine resolver of its own; CI
	// proves one and passes it in, as it does for the selected-port canary.
	if control := os.Getenv("AGENT_HARNESS_TEST_LOOPBACK_CONTROL"); control != "" {
		discover := edgeControl
		edgeControl = func(ctx context.Context, _ string) (sandboxprobe.ControlTarget, string) {
			return discover(ctx, control)
		}
		t.Cleanup(func() { edgeControl = discover })
	}
	for _, loopback := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "host-loopback"}[loopback], func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			o := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Binary: "/usr/bin/perl", Home: filepath.Join(root, "login")}}, WorkDir: filepath.Join(root, "work"), RuntimeHome: filepath.Join(root, "runtime"), Sandbox: &Sandbox{Write: true, Loopback: loopback, Edge: &Edge{}}}
			for _, dir := range []string{o.WorkDir, o.RuntimeHome, filepath.Join(o.RuntimeHome, "tmp"), filepath.Join(o.WorkDir, ".git"), filepath.Join(root, "outside"), o.Provider.CLI.Home} {
				if err = os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			p, err := edgeproxy.NewCanary(nil, edgeCanaryHost, "127.0.0.1:1")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			plan, closeBoundary, err := prepareEdgeBoundary(ctx, o, p, root)
			// The proof needs a reachable off-machine HTTPS and DNS control
			// to tell a denial from no network at all; a hosted runner may
			// have neither.
			var unavailable *CapabilityError
			if errors.As(err, &unavailable) && unavailable.Code == CapabilityEdgeControlUnavailable {
				testenv.Unavailable(t, "an off-machine network control", err)
			}
			if err != nil {
				t.Fatal("preparing the edge boundary", err)
			}
			defer closeBoundary()
			script := filepath.Join(o.WorkDir, "boundary.pl")
			if err = os.WriteFile(script, []byte(edgeBoundaryScript), 0600); err != nil {
				t.Fatal(err)
			}
			output, runErr := runOnce(ctx, plan.Binary, append(plan.Args, script), o.WorkDir, plan.Env)
			if err = judgeEdgeBoundary(o, output, runErr); err != nil {
				t.Fatal("actual Seatbelt boundary refused", err)
			}
			t.Logf("seatbelt edge=%s loopback=%t complete boundary observations=%s", edgeRevision, loopback, output)
		})
	}
}
