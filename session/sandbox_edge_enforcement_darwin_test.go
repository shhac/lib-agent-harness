package session

import (
	"context"
	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
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
			if err != nil {
				t.Fatal("unsandboxed control unavailable", err)
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
