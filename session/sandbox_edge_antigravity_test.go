package session

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func antigravityEdgeOptions(t *testing.T) Options {
	return Options{Provider: harness.Provider{Engine: harness.Antigravity}, WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Loopback: true, Edge: &Edge{GeminiAPIKey: "dummy-gemini-key"}}}
}

func TestEdgeOfferedExactlyWhereRegistered(t *testing.T) {
	for _, engine := range harness.Engines() {
		offered := harness.Support(engine, harness.Session, harness.SandboxEdge).Availability != harness.Unsupported
		if runtime.GOOS == "darwin" && offered != (engines[engine].edge != nil) {
			t.Fatalf("%s: support %v disagrees with the engine registry", engine, offered)
		}
		if runtime.GOOS != "darwin" && offered {
			t.Fatalf("%s: edge offered off macOS", engine)
		}
	}
	if engines[harness.Fx].edge != nil {
		t.Fatal("fx registered for an edge it cannot use")
	}
}

func TestAntigravityEdgeRefusals(t *testing.T) {
	if runtime.GOOS != "darwin" {
		var refused *UnsupportedError
		if _, err := normalizeEdge(antigravityEdgeOptions(t)); !errors.As(err, &refused) || refused.Code != RefusedEdgeUnavailable {
			t.Fatal("edge offered off macOS", err)
		}
		return
	}
	if _, err := normalizeEdge(antigravityEdgeOptions(t)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mutate func(*Options)
		want   string
	}{
		"key":      {func(o *Options) { o.Sandbox.Edge.GeminiAPIKey = "" }, RefusedEdgeTokenRequired},
		"bad-key":  {func(o *Options) { o.Sandbox.Edge.GeminiAPIKey = "a\nb" }, RefusedEdgeConflict},
		"loopback": {func(o *Options) { o.Sandbox.Loopback = false }, RefusedEdgeConflict},
		"web":      {func(o *Options) { o.Sandbox.Web = true }, RefusedEdgeConflict},
		"tools":    {func(o *Options) { o.Sandbox.Tools = &ToolHost{} }, RefusedEdgeConflict},
		"api-key":  {func(o *Options) { o.Env = []string{"GEMINI_API_KEY=other"} }, RefusedEdgeConflict},
		"base-url": {func(o *Options) { o.Env = []string{"GOOGLE_GEMINI_BASE_URL=https://elsewhere.example"} }, RefusedEdgeConflict},
		"trust":    {func(o *Options) { o.Env = []string{"SSL_CERT_FILE=/elsewhere"} }, RefusedEdgeConflict},
		"wildcard": {func(o *Options) { o.Sandbox.Edge.AllowHosts = []string{"*.googleapis.com"} }, RefusedEdgeConflict},
		"no-home":  {func(o *Options) { o.RuntimeHome = "" }, RefusedRuntimeHome},
		"skills":   {func(o *Options) { o.Skills.Provided = []harness.Skill{{Name: "demo", Dir: "/skills/demo"}} }, RefusedEdgeConflict},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := antigravityEdgeOptions(t)
			c.mutate(&o)
			var refused *UnsupportedError
			if _, err := normalizeEdge(o); !errors.As(err, &refused) || refused.Code != c.want {
				t.Fatalf("want %s, got %v", c.want, err)
			}
		})
	}
}

func TestAntigravityEdgeEnvironment(t *testing.T) {
	o := antigravityEdgeOptions(t)
	o.Sandbox.Edge.BaseURL = "https://gemini.example/v1"
	resolved, err := filepath.EvalSymlinks(o.RuntimeHome)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range edgeEnvironment(o, environmentEdgeProxy{}) {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	want := map[string]string{"HOME": resolved, "GEMINI_API_KEY": "dummy-gemini-key", "SSL_CERT_FILE": antigravityCABundle, "GOOGLE_GEMINI_BASE_URL": o.Sandbox.Edge.BaseURL, "HTTPS_PROXY": (environmentEdgeProxy{}).Endpoint(), "NO_PROXY": "localhost,127.0.0.1,::1"}
	for key, value := range want {
		if values[key] != value {
			t.Fatalf("%s = %q, want %q", key, values[key], value)
		}
	}
	for _, key := range []string{"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"} {
		if _, ok := values[key]; ok {
			t.Fatal("another engine's variable set", key)
		}
	}
	if args := edgeArgs(o); len(args) != 0 {
		t.Fatal("agy given edge arguments; its own --sandbox must never be passed", args)
	}
	for _, d := range edgeDestinations(o) {
		if d.Host != "generativelanguage.googleapis.com" && d.Host != "gemini.example" {
			t.Fatal("unexpected default host", d.Host)
		}
	}
}

func TestAntigravityEdgeCanaryWire(t *testing.T) {
	o := antigravityEdgeOptions(t)
	w := httptest.NewRecorder()
	request := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"view_file"},{"name":"run_command"}]}]}`
	if !edgeShellResponse(w, o, []byte(request), "canary-command") {
		t.Fatal("run_command not answered")
	}
	body := strings.TrimSpace(strings.TrimPrefix(w.Body.String(), "data: "))
	var chunk struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					FunctionCall struct {
						ID, Name string
						Args     map[string]any
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(body), &chunk); err != nil || len(chunk.Candidates) != 1 {
		t.Fatal("not one Gemini chunk", err, body)
	}
	call := chunk.Candidates[0].Content.Parts[0].FunctionCall
	if call.ID != "call_edge" || call.Name != "run_command" || call.Args["CommandLine"] != "canary-command" || call.Args["toolSummary"] == nil || call.Args["WaitMsBeforeAsync"] == nil {
		t.Fatal("wrong native call", call)
	}
	if edgeShellResponse(httptest.NewRecorder(), o, []byte(`{"tools":[{"functionDeclarations":[{"name":"view_file"}]}]}`), "canary-command") {
		t.Fatal("unproved shell surface accepted")
	}
	result := func(id, name string) string {
		return `{"contents":[{"role":"model","parts":[{"functionResponse":{"id":"` + id + `","name":"` + name + `","response":{"output":"marker"}}}]}]}`
	}
	if out, ok := edgeNativeResult(harness.Antigravity, []byte(result("call_edge", "run_command"))); !ok || out != "marker" {
		t.Fatal("own result not read", out, ok)
	}
	for _, other := range []string{result("other", "run_command"), result("call_edge", "view_file"), request} {
		if _, ok := edgeNativeResult(harness.Antigravity, []byte(other)); ok {
			t.Fatal("another call's result read as the canary's", other)
		}
	}
}

func TestAntigravityEdgeHomeIsReadOnlyBeyondItsState(t *testing.T) {
	if len(antigravityEdge.protectRuntime) != 1 || antigravityEdge.protectRuntime[0] != ".gemini" {
		t.Fatal("agy's home is not protected whole", antigravityEdge.protectRuntime)
	}
	writable := func(path string) bool {
		for _, entry := range antigravityEdge.writableRuntime {
			if strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry) || path == entry || !strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry) && !strings.Contains(path[len(entry):], "/") {
				return true
			}
		}
		return false
	}
	loaders := []string{"settings", "mcp_config", "hooks", "plugin", "skills", "rules", "agents", "workflows", "sidecars", "builtin", "GEMINI.md", "AGENTS.md", "config.json", "webm_encoder"}
	for _, entry := range antigravityEdge.writableRuntime {
		if !strings.HasPrefix(entry, ".gemini/") {
			t.Fatal("writable state outside agy's home", entry)
		}
		for _, loader := range loaders {
			if strings.Contains(entry, loader) {
				t.Fatal("a loader is writable", entry)
			}
		}
		if strings.HasPrefix(entry, ".gemini/antigravity-cli/bin") && entry != ".gemini/antigravity-cli/bin/agentapi" {
			t.Fatal("agy's executables writable", entry)
		}
	}
	if len(antigravityEdge.canary.unlisted) == 0 {
		t.Fatal("no unlisted loader for the canary to try")
	}
	for _, sample := range antigravityEdge.canary.unlisted {
		if !strings.HasPrefix(sample, ".gemini/") || writable(sample) {
			t.Fatal("canary sample the protection does not cover", sample)
		}
	}
}
