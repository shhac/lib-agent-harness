//go:build !windows

package session

import (
	"context"
	"errors"
	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type failingEdgeProxy struct {
	*edgeproxy.Proxy
	failed chan struct{}
}

func (p *failingEdgeProxy) Failed() <-chan struct{} { return p.failed }

// Lifecycle-only fixture. It substitutes the command plan, not public proof,
// so proxy failure coverage needs process containment but no nested Seatbelt.
func TestEdgeProxyFailureFailsActiveTurn(t *testing.T) {
	testenv.RequireLoopback(t)
	testenv.RequireProcessGroup(t)
	for _, scenario := range []struct {
		engine  harness.Engine
		refresh bool
	}{{harness.Codex, false}, {harness.Claude, false}, {harness.Codex, true}} {
		engine := scenario.engine
		name := string(engine)
		if scenario.refresh {
			name += "-expired-login"
		}
		t.Run(name, func(t *testing.T) {
			binary, _ := fakeHarness(t, "edge-session")
			o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: t.TempDir()}}, WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Write: true, Edge: &Edge{ClaudeToken: "dummy"}}}
			var providerCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { providerCalls.Add(1); w.WriteHeader(500) }))
			defer provider.Close()
			p, err := edgeproxy.NewCanary(edgeDestinations(o), edgeCanaryHost, provider.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			owner := []byte(`{"expires_at":1,"refresh_token":"dummy-rotating-token"}`)
			for _, dir := range []string{o.Provider.CLI.Home, o.RuntimeHome} {
				if err := os.WriteFile(filepath.Join(dir, "auth.json"), owner, 0600); err != nil {
					t.Fatal(err)
				}
			}
			failing := &failingEdgeProxy{Proxy: p, failed: make(chan struct{})}
			identity, err := edgeIdentity(o)
			if err != nil {
				t.Fatal(err)
			}
			env := edgeEnvironment(o, p)
			env = append(env, "HOME="+o.RuntimeHome)
			l := &launch{extra: edgeArgs(o), edge: &edgeLaunch{proxy: failing, identity: identity, plan: sandboxbridge.EdgePlan{Binary: binary, Env: env}}}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s := &Session{options: o, ref: reference(o, newID()), caps: CapabilitiesFor(engine), lifetime: ctx, done: make(chan struct{}), opGate: make(chan struct{}, 1)}
			s.mu.Lock()
			w, err := newProcessWire(ctx, o, s.ref.ID, false, l, nil, s.notification, s.fail)
			s.transport = w
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.initialize(ctx, false); err != nil {
				t.Fatal(err)
			}
			prompt, want := "edge-fixture-wait", CapabilityEdgeProxyFailed
			if scenario.refresh {
				prompt, want = "edge-fixture-refresh", CapabilityEdgeLoginRefreshBlocked
			}
			turn, err := s.StartTurn(ctx, Input{Text: prompt})
			if err != nil && !scenario.refresh {
				t.Fatal(err)
			}
			if !scenario.refresh {
				close(failing.failed)
			}
			if err == nil {
				_, err = turn.Wait(ctx)
			}
			var capability *CapabilityError
			if !errors.As(err, &capability) || capability.Code != want {
				t.Fatal("active turn lost proxy failure", err)
			}
			s.awaitReaped(ctx)
			if !s.reaped() {
				t.Fatal("CLI not reaped")
			}
			if scenario.refresh {
				got, err := os.ReadFile(filepath.Join(o.Provider.CLI.Home, "auth.json"))
				if err != nil || string(got) != string(owner) || providerCalls.Load() != 0 {
					t.Fatal("refresh consumed or changed owner login", err)
				}
			}
			endpoint, _ := url.Parse(p.Endpoint())
			if conn, e := net.DialTimeout("tcp", endpoint.Host, time.Second); e == nil {
				conn.Close()
				t.Fatal("failed proxy remained reachable")
			}
		})
	}
}
