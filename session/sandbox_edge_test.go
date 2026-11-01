package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

type environmentEdgeProxy struct{}

func (environmentEdgeProxy) Endpoint() string        { return "http://dummy:secret@127.0.0.1:1234" }
func (environmentEdgeProxy) Credential() string      { return "secret" }
func (environmentEdgeProxy) Failed() <-chan struct{} { return nil }
func (environmentEdgeProxy) Close()                  {}

func TestEdgeEnvironmentPreservesBothProxySpellings(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		o := Options{Provider: harness.Provider{Engine: harness.Claude}, RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Loopback: loopback, Edge: &Edge{ClaudeToken: "dummy"}}}
		values := map[string]string{}
		for _, entry := range edgeEnvironment(o, environmentEdgeProxy{}) {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
		}
		for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if values[key] != (environmentEdgeProxy{}).Endpoint() {
				t.Fatalf("missing %s", key)
			}
		}
		for _, key := range []string{"DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY", "MAX_RETRIES"} {
			want := "1"
			if key == "MAX_RETRIES" {
				want = "0"
			}
			if values[key] != want {
				t.Fatalf("missing production switch %s", key)
			}
		}
		if _, ok := values["HARNESS_PROBE_KEY"]; ok {
			t.Fatal("probe-only provider selector")
		}
		want := ""
		if loopback {
			want = "localhost,127.0.0.1,::1"
		}
		for _, key := range []string{"NO_PROXY", "no_proxy"} {
			if value, ok := values[key]; !ok || value != want {
				t.Fatalf("incorrect %s", key)
			}
		}
	}
}

func TestEdgeUnavailableControlIsNotEscape(t *testing.T) {
	old := edgeControl
	defer func() { edgeControl = old }()
	edgeControl = func(context.Context, string) (sandboxprobe.ControlTarget, string) {
		return sandboxprobe.ControlTarget{}, "offline"
	}
	o := Options{Provider: harness.Provider{Engine: harness.Codex}}
	_, close, err := prepareEdgeBoundary(context.Background(), o, nil, "")
	close()
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityEdgeControlUnavailable || failure.Reason == "observed_escape" {
		t.Fatal("unavailable control reported as escape", err)
	}
}

func TestEdgeRefreshHostCannotBeGranted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		return
	}
	for _, edge := range []*Edge{{AllowHosts: []string{"AUTH.OPENAI.COM:443"}}, {BaseURL: "https://auth.openai.com"}} {
		o := Options{Provider: harness.Provider{Engine: harness.Codex}, RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Edge: edge}}
		_, err := normalizeEdge(o)
		var refused *UnsupportedError
		if !errors.As(err, &refused) || refused.Code != RefusedEdgeConflict {
			t.Fatal("refresh host admitted", err)
		}
	}
}

func TestEdgeNativeNamespaceCall(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, WorkDir: "/work"}
	w := httptest.NewRecorder()
	if !edgeShellResponse(w, o, []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command"}]}]}`), "canary-command") {
		t.Fatal("namespace omitted")
	}
	if !strings.Contains(w.Body.String(), `"namespace":"functions"`) || !strings.Contains(w.Body.String(), `"name":"exec_command"`) {
		t.Fatal("wrong native call")
	}
	if edgeShellResponse(httptest.NewRecorder(), o, []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read_file"}]}]}`), "canary-command") {
		t.Fatal("unproved shell surface accepted")
	}
}

func TestEdgeCancelledProofIsNotCached(t *testing.T) {
	old := edgeProbe
	defer func() { edgeProbe = old }()
	calls := 0
	edgeProbe = func(ctx context.Context, _ Options) error { calls++; <-ctx.Done(); return ctx.Err() }
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, Sandbox: &Sandbox{Edge: &Edge{}}}
	l := &launch{edge: &edgeLaunch{identity: t.Name(), plan: sandboxbridge.EdgePlan{Key: t.Name()}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var timedOut *CapabilityError
	if err := verifyEdge(ctx, o, l); !errors.As(err, &timedOut) || timedOut.Code != CapabilityProbeTimeout {
		t.Fatal(err)
	}
	edgeProbe = func(context.Context, Options) error { calls++; return nil }
	if err := verifyEdge(context.Background(), o, l); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("cancelled evidence was reused", calls)
	}
	if err := verifyEdge(context.Background(), o, l); err != nil || calls != 2 {
		t.Fatal("settled evidence not reused", err, calls)
	}
}

func TestEdgeConcurrentProofSerializes(t *testing.T) {
	old := edgeProbe
	defer func() { edgeProbe = old }()
	entered, release := make(chan struct{}), make(chan struct{})
	edgeProbe = func(context.Context, Options) error { close(entered); <-release; return nil }
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, Sandbox: &Sandbox{Edge: &Edge{}}}
	l := &launch{edge: &edgeLaunch{identity: t.Name(), plan: sandboxbridge.EdgePlan{Key: t.Name()}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- verifyEdge(ctx, o, l) }()
	<-entered
	go func() { results <- verifyEdge(ctx, o, l) }()
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestEdgePreflightConflicts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		return
	}
	old := edgeProbe
	defer func() { edgeProbe = old }()
	probes := 0
	edgeProbe = func(context.Context, Options) error { probes++; return nil }
	cases := map[string]func(*Options){
		"browser":     func(o *Options) { o.Browser = true },
		"restriction": func(o *Options) { o.Restriction = &Restriction{} },
		"local-only":  func(o *Options) { o.Sandbox.LoopbackLocalOnly = true },
		"ports":       func(o *Options) { o.Sandbox.Loopback = true; o.Sandbox.LoopbackPorts = []int{3000} },
		"runtime":     func(o *Options) { o.RuntimeHome = "" },
		"token":       func(o *Options) { o.Sandbox.Edge.ClaudeToken = "" },
		"web":         func(o *Options) { o.Sandbox.Web = true },
		"wildcard":    func(o *Options) { o.Sandbox.Edge.AllowHosts = []string{"*.example.com"} },
		"url":         func(o *Options) { o.Sandbox.Edge.BaseURL = "http://example.com" },
	}
	for _, key := range []string{"HTTPS_PROXY", "http_proxy", "NO_PROXY", "HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"} {
		cases[key] = func(o *Options) { o.Env = []string{key + "=forbidden"} }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := Options{Provider: harness.Provider{Engine: harness.Claude}, RuntimeHome: "/unavailable-private-home", Sandbox: &Sandbox{Edge: &Edge{ClaudeToken: "dummy"}}}
			state := t.TempDir()
			o.RuntimeHome = state
			mutate(&o)
			_, err := Start(context.Background(), o)
			want := RefusedEdgeConflict
			switch name {
			case "token":
				want = RefusedEdgeTokenRequired
			case "runtime":
				want = RefusedRuntimeHome
			case "ports":
				want = RefusedLoopbackPortsUnenforceable
			}
			var refused *UnsupportedError
			if !errors.As(err, &refused) || refused.Code != want {
				t.Fatalf("want %s, got %v", want, err)
			}
			if probes != 0 {
				t.Fatal("preflight ran probe")
			}
			entries, e := os.ReadDir(state)
			if e != nil || len(entries) != 0 {
				t.Fatal("preflight prepared runtime state", e)
			}
		})
	}
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, RuntimeHome: "/unused", Sandbox: &Sandbox{Web: true, Edge: &Edge{}}}
	if _, err := normalizeEdge(o); err != nil {
		t.Fatal("Codex provider-side web refused", err)
	}
}

func TestEdgePlatformRefusesBeforeState(t *testing.T) {
	if runtime.GOOS == "darwin" {
		return
	}
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok, harness.CommandCode, harness.OpenAICompatible} {
		o := Options{Provider: harness.Provider{Engine: engine}, RuntimeHome: "/not-a-directory", Sandbox: &Sandbox{Edge: &Edge{ClaudeToken: "dummy"}}}
		for _, call := range []func() error{
			func() error { _, e := Start(context.Background(), o); return e },
			func() error { _, e := Resume(context.Background(), o, Ref{}); return e },
			func() error { return VerifySandbox(context.Background(), o) },
		} {
			var e *UnsupportedError
			if err := call(); !errors.As(err, &e) || e.Code != RefusedEdgeUnavailable {
				t.Fatalf("%s: %v", engine, err)
			}
		}
	}
}

func TestEdgeReferencePolicyAndSecrets(t *testing.T) {
	base := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Binary: "/cli", Home: "/login"}}, WorkDir: "/work", RuntimeHome: "/runtime", Sandbox: &Sandbox{}}
	legacy := reference(base, "id")
	base.Sandbox.Edge = &Edge{AllowHosts: []string{"a.example:443"}, ClaudeToken: "secret-one"}
	first := reference(base, "id")
	if first.ConfigHash == legacy.ConfigHash {
		t.Fatal("edge not bound")
	}
	base.Sandbox.Edge.ClaudeToken = "secret-two"
	if reference(base, "id").ConfigHash != first.ConfigHash {
		t.Fatal("token entered reference")
	}
	bytes, _ := json.Marshal(first)
	if strings.Contains(string(bytes), "secret-") {
		t.Fatal("secret in ref")
	}
	base.Sandbox.Edge.AllowHosts = []string{"b.example:443"}
	if reference(base, "id").ConfigHash == first.ConfigHash {
		t.Fatal("host policy not bound")
	}
	base.Sandbox.Edge.AllowHosts = []string{"a.example:443"}
	base.Sandbox.Edge.BaseURL = "https://a.example/v1"
	if reference(base, "id").ConfigHash == first.ConfigHash {
		t.Fatal("base URL not bound")
	}
	base.Sandbox.Edge = nil
	if !reflect.DeepEqual(reference(base, "id"), legacy) {
		t.Fatal("legacy ref changed")
	}
}

func TestEdgeEnginePolicy(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, Sandbox: &Sandbox{Edge: &Edge{}}, WorkDir: "/work"}
	p := codexThreadParams(o, o.WorkDir, false, "")
	if p["sandbox"] != "danger-full-access" || p["approvalPolicy"] != "never" {
		t.Fatal(p)
	}
	if err := checkCodexSandbox(o, []byte(`{"sandbox":{"type":"dangerFullAccess"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := checkCodexSandbox(o, []byte(`{"sandbox":{"type":"readOnly"}}`)); err == nil {
		t.Fatal("wrong policy accepted")
	}
	for _, arg := range edgeArgs(o) {
		if strings.HasPrefix(arg, "default_permissions=") || strings.HasPrefix(arg, "permissions.") {
			t.Fatal("native profile retained")
		}
	}
	o.Provider.Engine = harness.Claude
	var settings struct {
		Sandbox struct {
			Enabled bool `json:"enabled"`
		} `json:"sandbox"`
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if json.Unmarshal([]byte(claudeSandboxSettings(o)), &settings) != nil || settings.Sandbox.Enabled {
		t.Fatal("Claude nested sandbox")
	}
	if !strings.Contains(claudeSandboxSettings(o), `"Bash"`) {
		t.Fatal("Claude shell not admitted")
	}
}

func TestEdgeJudgmentFailsClosed(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, Sandbox: &Sandbox{Edge: &Edge{}}}
	complete := "read-inside read-outside-denied write-outside-denied exec-outside-denied grandchild-denied git-denied claude-config-denied mcp-config-denied runtime-config-denied runtime-auth-denied runtime-record-denied loader-claude-agents-denied loader-claude-commands-denied loader-claude-skills-denied loader-prompts-denied loader-rules-denied proxy-denied tcp443-denied tcp53-denied udp53-denied dns-denied finished write-inside-denied host-denied"
	if err := judgeEdgeBoundary(o, []byte(complete), nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range strings.Fields(complete) {
		if err := judgeEdgeBoundary(o, []byte(strings.ReplaceAll(complete, key, "")), nil); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	for _, escape := range []string{"escape-network-tcp53", "escape-dns", "escape-read"} {
		var e *CapabilityError
		err := judgeEdgeBoundary(o, []byte(complete+" "+escape), errors.New("interrupted"))
		if !errors.As(err, &e) || e.Reason != "observed_escape" {
			t.Fatal("escape lost", err)
		}
	}
}

func TestEdgeResultCannotEchoCommandMarkers(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		if _, ok := edgeNativeResult(engine, []byte(`{"prompt":"finished read-inside proxy-denied"}`)); ok {
			t.Fatal("prompt claimed execution")
		}
	}
	text, ok := edgeNativeResult(harness.Codex, []byte(`{"input":[{"type":"function_call_output","call_id":"call_edge","output":"finished"}]}`))
	if !ok || text != "finished" {
		t.Fatal("native result lost")
	}
	text, ok = edgeNativeResult(harness.Claude, []byte(`{"messages":[{"content":[{"type":"tool_result","tool_use_id":"tool_edge","content":[{"type":"text","text":"finished"}]}]}]}`))
	if !ok || strings.TrimSpace(text) != "finished" {
		t.Fatal("Claude result lost")
	}
}

func TestEdgeFactsAndScrubbing(t *testing.T) {
	for code, want := range map[string]string{CapabilityEdgeControlUnavailable: "edge_control_unavailable", CapabilityEdgeLoginRefreshBlocked: "edge_login_refresh_blocked"} {
		if code != want {
			t.Fatal("edge fact changed")
		}
		facts, ok := harness.ErrorFacts(&CapabilityError{Engine: harness.Codex, Code: code, Phase: DuringSession})
		if !ok || facts.Code != want {
			t.Fatal("edge fact lost")
		}
	}
	codes := map[string]string{RefusedEdgeUnavailable: "edge_unavailable", RefusedEdgeProxyUnavailable: "edge_proxy_unavailable", RefusedEdgeConflict: "edge_conflict", RefusedEdgeTokenRequired: "edge_token_required", CapabilityEdgeProviderPath: "edge_provider_path", CapabilityEdgeEgressNotRefused: "edge_egress_not_refused", CapabilityEdgeDNSNotRefused: "edge_dns_not_refused", CapabilityEdgeConfinementFailed: "edge_confinement_failed", CapabilityEdgeProxyFailed: "edge_proxy_failed"}
	for code, want := range codes {
		if code != want {
			t.Fatal("code changed")
		}
		e := &CapabilityError{Engine: harness.Claude, Code: code, Phase: BeforeLaunch}
		facts, ok := harness.ErrorFacts(e)
		if !ok || facts.Code != code {
			t.Fatal("fact lost")
		}
	}
	secret := `token"with\escape`
	encoded, _ := json.Marshal(secret)
	clean := scrubEdge(encoded, []string{secret})
	if !json.Valid(clean) || strings.Contains(string(clean), "token") {
		t.Fatal("escaped secret leaked")
	}
}
