package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/textbound"
	"github.com/shhac/lib-agent-harness/process"
)

// This test is explicitly selected in the pinned npm macOS CI step. An absent
// selection is not installed-CLI evidence; when selected nothing may skip.
// AGENT_HARNESS_EDGE_CLI_CANARY_ENGINES names the engines to run, comma
// separated; it defaults to the pinned Codex and Claude.
func TestEdgeInstalledCLICanary(t *testing.T) {
	if os.Getenv("AGENT_HARNESS_EDGE_CLI_CANARY") != "1" {
		return
	}
	useTestControl(t)
	for _, engine := range edgeCanaryEngines() {
		if engine == harness.Fx {
			continue
		}
		t.Run(string(engine), func(t *testing.T) {
			described, _ := harness.Describe(engine)
			binary, err := exec.LookPath(described.Binary)
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
			switch engine {
			case harness.Claude:
				model = "haiku"
			case harness.Antigravity:
				// agy has a fixed login home and chooses its own model.
				login, model = "", ""
			}
			o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: login}}, WorkDir: work, RuntimeHome: runtimeHome, Model: model, Sandbox: &Sandbox{Write: true, Loopback: true, Edge: &Edge{ClaudeToken: "sk-ant-oat01-dummy-edge-canary-only", GeminiAPIKey: "dummy-edge-gemini-canary-only"}}}
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
				t.Log("confined version diagnostic:", sanitize(scrubEdge(diagnostic.Bytes(), []string{refusing.Credential(), refusing.Endpoint(), base64.StdEncoding.EncodeToString([]byte("edge:" + refusing.Credential())), o.Sandbox.Edge.ClaudeToken, o.Sandbox.Edge.GeminiAPIKey}), 2048))
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
				var failure *CapabilityError
				if errors.As(err, &failure) {
					t.Fatalf("installed edge canary refused: %v (code %s, reason %q)", err, failure.Code, failure.Reason)
				}
				t.Fatal("installed edge canary refused", err)
			}
		})
	}
}

func edgeCanaryEngines() []harness.Engine {
	selected := os.Getenv("AGENT_HARNESS_EDGE_CLI_CANARY_ENGINES")
	if selected == "" {
		return []harness.Engine{harness.Codex, harness.Claude}
	}
	var out []harness.Engine
	for _, name := range strings.Split(selected, ",") {
		out = append(out, harness.Engine(strings.TrimSpace(name)))
	}
	return out
}

// fx is refused the edge because it does not use the provider proxy. This
// keeps that reason tied to the installed binary: with every proxy variable
// naming a local listener and all off-machine egress denied, fx neither
// connects to the listener nor completes its request. No credential is real
// and nothing leaves the machine.
func TestEdgeInstalledFxIgnoresProxy(t *testing.T) {
	if os.Getenv("AGENT_HARNESS_EDGE_CLI_CANARY") != "1" || !slices.Contains(edgeCanaryEngines(), harness.Fx) {
		return
	}
	binary, err := exec.LookPath("fx")
	if err != nil {
		t.Fatal("fx missing", err)
	}
	if c := harness.Support(harness.Fx, harness.Session, harness.SandboxEdge); c.Usable() || c.Reason != harness.SandboxEdgeFxReason {
		t.Fatal("fx edge support changed without evidence", c)
	}
	o := Options{Provider: harness.Provider{Engine: harness.Fx, CLI: harness.CLI{Binary: binary}}, WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Write: true, Loopback: true, Edge: &Edge{}}}
	var refused *UnsupportedError
	if _, err = normalize(o); !errors.As(err, &refused) || refused.Code != RefusedEdgeUnavailable {
		t.Fatal("fx admitted to the edge", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var connects atomic.Int32
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			connects.Add(1)
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
			conn.Close()
		}
	}()
	home := t.TempDir()
	proxy := "http://" + listener.Addr().String()
	profile := `(version 1)(allow default)(deny network-outbound (remote ip "*:*"))(allow network-outbound (remote ip "localhost:*"))(deny mach-lookup (global-name "com.apple.SecurityServer") (global-name "com.apple.securityd"))`
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, tree, err := process.Command(ctx, "/usr/bin/sandbox-exec", "-p", profile, binary, "ask", "Local capability canary only.")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	cmd.Dir, cmd.WaitDelay = o.WorkDir, 2*time.Second
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + home, "HTTPS_PROXY=" + proxy, "https_proxy=" + proxy, "HTTP_PROXY=" + proxy, "http_proxy=" + proxy, "ALL_PROXY=" + proxy, "all_proxy=" + proxy, "FX_DISABLE_KEYCHAIN=1", "FX_NO_OPEN_BROWSER=1", "FX_SKIP_ONBOARDING=1", "AI_GATEWAY_API_KEY=dummy-edge-gateway-key-not-a-login"}
	// The same profile and environment must run fx itself, or a refused request
	// would prove nothing.
	control := exec.Command("/usr/bin/sandbox-exec", "-p", profile, binary, "--version")
	control.Env = cmd.Env
	version, err := control.Output()
	if err != nil || !strings.Contains(string(version), ".") {
		t.Fatal("fx does not run under the control profile", err)
	}
	bytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("engine=fx version=%s binary_sha256=%x", strings.TrimSpace(sanitize(version, 256)), sha256.Sum256(bytes))
	diagnostic := &textbound.Prefix{Limit: 2048}
	cmd.Stderr = diagnostic
	if err = tree.Run(); err == nil {
		t.Fatal("fx completed a request with only the proxy reachable")
	}
	t.Log("fx refusal:", strings.TrimSpace(sanitize(diagnostic.Bytes(), 256)))
	if n := connects.Load(); n != 0 {
		t.Fatalf("fx used the proxy (%d connections): its edge refusal is stale", n)
	}
}
