package session

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestEdgeFixtureLifecycleAndSecrets(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	testenv.RequireLoopback(t)
	testenv.RequireProcessGroup(t)
	old := edgeProbe
	edgeProbe = func(context.Context, Options) error { return nil }
	defer func() { edgeProbe = old }()
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
			binary, _ := fakeHarness(t, "edge-session")
			o := sandboxOptions(t, engine, binary, true)
			o.RuntimeHome = t.TempDir()
			if err := os.Chmod(o.RuntimeHome, 0700); err != nil {
				t.Fatal(err)
			}
			if engine == harness.Claude {
				o.Provider.CLI.Home = t.TempDir()
			}
			o.Sandbox.Edge = &Edge{ClaudeToken: "sk-ant-oat01-fixture-secret-only"}
			o.Sandbox.Read = []string{filepath.Dir(os.Args[0])}
			var mu sync.Mutex
			var diagnostics []string
			o.OnDiagnostic = func(d Diagnostic) { mu.Lock(); diagnostics = append(diagnostics, d.Detail); mu.Unlock() }
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			data, err := os.ReadFile(filepath.Join(o.RuntimeHome, "edge-fixture-env.json"))
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]string
			if json.Unmarshal(data, &env) != nil {
				t.Fatal("fixture env invalid")
			}
			proxy, err := url.Parse(env["proxy"])
			if err != nil {
				t.Fatal(err)
			}
			credential, _ := proxy.User.Password()
			if engine == harness.Claude {
				resolved, err := filepath.EvalSymlinks(o.RuntimeHome)
				if err != nil {
					t.Fatal(err)
				}
				if env["config"] != filepath.Join(resolved, "claude") {
					t.Fatal("private Claude config path not selected")
				}
				if env["token"] != o.Sandbox.Edge.ClaudeToken {
					t.Fatal("private Claude token not selected")
				}
			}
			ref := s.Ref()
			refBytes, _ := json.Marshal(ref)
			if strings.Contains(string(refBytes), credential) || strings.Contains(string(refBytes), o.Sandbox.Edge.ClaudeToken) {
				t.Fatal("reference leaked secret")
			}
			turn, err := s.StartTurn(ctx, Input{Text: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			for e := range turn.Events() {
				bytes, _ := json.Marshal(e)
				if strings.Contains(string(bytes), credential) || strings.Contains(string(bytes), o.Sandbox.Edge.ClaudeToken) {
					t.Fatal("event leaked secret")
				}
			}
			if _, err = turn.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s.awaitReaped(ctx)
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			if conn, err := net.DialTimeout("tcp", proxy.Host, time.Second); err == nil {
				conn.Close()
				t.Fatal("proxy survived process reap")
			}
			mu.Lock()
			for _, text := range diagnostics {
				if strings.Contains(text, credential) || strings.Contains(text, o.Sandbox.Edge.ClaudeToken) {
					t.Error("diagnostic leaked secret")
				}
			}
			mu.Unlock()
			changed := o
			sb := *o.Sandbox
			edge := *sb.Edge
			edge.AllowHosts = []string{"different.example:443"}
			sb.Edge = &edge
			changed.Sandbox = &sb
			if _, err = Resume(ctx, changed, ref); !errors.Is(err, ErrIncompatibleResume) {
				t.Fatal("policy mismatch did not refuse", err)
			}
			resumed, err := Resume(ctx, o, ref)
			if err != nil {
				t.Fatal("compatible edge resume", err)
			}
			defer resumed.Close()
			data, err = os.ReadFile(filepath.Join(o.RuntimeHome, "edge-fixture-env.json"))
			if err != nil || json.Unmarshal(data, &env) != nil {
				t.Fatal("resumed private state missing", err)
			}
			fresh, err := url.Parse(env["proxy"])
			if err != nil || fresh.String() == proxy.String() {
				t.Fatal("resume reused proxy credentials", err)
			}
			crashed, err := resumed.StartTurn(ctx, Input{Text: "edge-fixture-crash"})
			if err == nil {
				_, err = crashed.Wait(ctx)
			}
			if err == nil {
				t.Fatal("CLI crash reported success")
			}
			resumed.awaitReaped(ctx)
			if conn, err := net.DialTimeout("tcp", fresh.Host, time.Second); err == nil {
				conn.Close()
				t.Fatal("proxy survived CLI crash")
			}
		})
	}
}
