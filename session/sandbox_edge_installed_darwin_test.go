package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/textbound"
	"github.com/shhac/lib-agent-harness/process"
)

// This test is explicitly selected in the pinned npm macOS CI step. An absent
// selection is not installed-CLI evidence; when selected nothing may skip.
func TestEdgeInstalledCLICanary(t *testing.T) {
	if os.Getenv("AGENT_HARNESS_EDGE_CLI_CANARY") != "1" {
		return
	}
	useTestControl(t)
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
			binary, err := exec.LookPath(string(engine))
			if err != nil {
				t.Fatal("pinned CLI missing", err)
			}
			runtimeHome := t.TempDir()
			if err := os.Chmod(runtimeHome, 0700); err != nil {
				t.Fatal(err)
			}
			login := t.TempDir()
			work := t.TempDir()
			model := "gpt-5.4"
			if engine == harness.Claude {
				model = "haiku"
			}
			o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: login}}, WorkDir: work, RuntimeHome: runtimeHome, Model: model, Sandbox: &Sandbox{Write: true, Loopback: true, Edge: &Edge{ClaudeToken: "sk-ant-oat01-dummy-edge-canary-only"}}}
			// The CI caller explicitly admits its Node install when outside the
			// shared system set; production never adds a PATH entry as a read grant.
			if node, e := exec.LookPath("node"); e == nil {
				if node, e = filepath.EvalSymlinks(node); e == nil {
					o.Sandbox.Read = []string{filepath.Dir(node)}
				}
			}
			o, err = normalize(o)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			// Version collection is confined too, with a proxy admitting no hosts.
			if err = os.MkdirAll(filepath.Join(runtimeHome, "tmp"), 0700); err != nil {
				t.Fatal(err)
			}
			refusing, err := edgeproxy.NewCanary(nil, edgeCanaryHost, "127.0.0.1:1")
			if err != nil {
				t.Fatal(err)
			}
			defer refusing.Close()
			plan, err := edgePlan(o, refusing, "")
			if err != nil {
				t.Fatal(err)
			}
			cmd, tree, err := process.Command(ctx, plan.Binary, append(plan.Args, "--version")...)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			out, diagnostic := &textbound.Prefix{Limit: 2048}, &textbound.Prefix{Limit: 2048}
			cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr, cmd.WaitDelay = work, plan.Env, out, diagnostic, 2*time.Second
			err = tree.Run()
			tree.Close()
			version := out.Bytes()
			if err != nil {
				t.Log("confined version diagnostic:", sanitize(scrubEdge(diagnostic.Bytes(), []string{refusing.Credential(), refusing.Endpoint(), base64.StdEncoding.EncodeToString([]byte("edge:" + refusing.Credential())), o.Sandbox.Edge.ClaudeToken}), 2048))
				t.Fatal("CLI version unavailable", err)
			}
			resolved, _, err := binaryIdentity(o)
			if err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(resolved)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("engine=%s version=%s binary_sha256=%x", engine, strings.TrimSpace(sanitize(version, 256)), sha256.Sum256(bytes))
			identity, err := edgeIdentity(o)
			if err != nil {
				t.Fatal("native executable identity unavailable", err)
			}
			t.Logf("engine=%s edge_executable_identity=%s (includes recognized native vendor and helper executables)", engine, identity)
			if err = probeEdge(ctx, o); err != nil {
				t.Fatal("installed edge canary refused", err)
			}
		})
	}
}
