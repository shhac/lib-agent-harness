package session

import (
	"context"
	"encoding/json"
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

// An Antigravity edge session through Start and Resume, with a fake agy in
// place of the real one and the proof substituted: the library prepares the
// API-key login, keeps the key and proxy credential out of references, events
// and diagnostics, resumes the same conversation over a fresh proxy, and
// closes the proxy when agy exits.
func TestEdgeFixtureAntigravityLifecycle(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	testenv.RequireLoopback(t)
	testenv.RequireProcessGroup(t)
	old := edgeProbe
	edgeProbe = func(context.Context, Options) error { return nil }
	defer func() { edgeProbe = old }()
	binary, _ := fakeHarness(t, "edge-session")
	o := sandboxOptions(t, harness.Antigravity, binary, true)
	o.RuntimeHome = t.TempDir()
	if err := os.Chmod(o.RuntimeHome, 0700); err != nil {
		t.Fatal(err)
	}
	secret := "fixture-gemini-key-secret-only"
	o.Sandbox.Loopback = true
	o.Sandbox.Edge = &Edge{GeminiAPIKey: secret}
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
	readEnv := func() map[string]string {
		data, err := os.ReadFile(filepath.Join(o.RuntimeHome, "edge-fixture-env.json"))
		var env map[string]string
		if err != nil || json.Unmarshal(data, &env) != nil {
			t.Fatal("fixture env missing", err)
		}
		return env
	}
	env := readEnv()
	proxy, err := url.Parse(env["proxy"])
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := proxy.User.Password()
	resolved, _ := filepath.EvalSymlinks(o.RuntimeHome)
	if env["home"] != resolved || env["token"] != secret {
		t.Fatal("private home or API key not selected", env["home"])
	}
	ref := s.Ref()
	if ref.ID != "edge-fixture-conversation" {
		t.Fatal("conversation not taken from init", ref.ID)
	}
	leaked := func(text string) bool { return strings.Contains(text, credential) || strings.Contains(text, secret) }
	if refBytes, _ := json.Marshal(ref); leaked(string(refBytes)) {
		t.Fatal("reference leaked secret")
	}
	turn, err := s.StartTurn(ctx, Input{Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for e := range turn.Events() {
		if bytes, _ := json.Marshal(e); leaked(string(bytes)) {
			t.Fatal("event leaked secret")
		}
	}
	if _, err = turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.awaitReaped(ctx)
	if conn, err := net.DialTimeout("tcp", proxy.Host, time.Second); err == nil {
		conn.Close()
		t.Fatal("proxy survived process reap")
	}
	resumed, err := Resume(ctx, o, ref)
	if err != nil {
		t.Fatal("compatible edge resume", err)
	}
	defer resumed.Close()
	if resumed.Ref().ID != ref.ID {
		t.Fatal("resume did not continue the conversation")
	}
	fresh, err := url.Parse(readEnv()["proxy"])
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
	mu.Lock()
	defer mu.Unlock()
	for _, text := range diagnostics {
		if leaked(text) {
			t.Error("diagnostic leaked secret")
		}
	}
}
