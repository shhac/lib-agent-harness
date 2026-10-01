package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/tomltest"
)

func TestSandboxBrowserBridgeDeclaration(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"name": "node_repl", "enabled": true, "transport": map[string]any{"type": "stdio", "command": filepath.Join(dir, "node_repl"), "args": []string{}, "env": map[string]string{
		"NODE_REPL_NODE_PATH": filepath.Join(dir, "node"), "NODE_REPL_NODE_MODULE_DIRS": dir, "NODE_REPL_TRUSTED_SERVICES": string(mustMarshal(map[string]string{"browser": filepath.Join(dir, "browser-service.mjs"), "sky": "@oai/sky/service"})), "NODE_REPL_TRUSTED_CODE_PATHS": "owner-home", "SECRET_TOKEN": "secret", "NODE_OPTIONS": "unsafe", "BROWSER_USE_SECURITY_MODE": "disabled-for-local-testing", "BROWSER_USE_AVAILABLE_BACKENDS": "chrome,iab,mcpapps", "CODEX_HOME": "owner-home", "BROWSER_USE_CODEX_APP_VERSION": "quoted\"\n\x01version",
	}}})
	bridge, err := parseCodexBrowserBridge(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := bridge.config(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"owner-home", "secret", "unsafe", "sky", "iab", "mcpapps", "disabled-for-local-testing"} {
		if strings.Contains(string(cfg), bad) {
			t.Fatalf("inherited %q", bad)
		}
	}
	if strings.Count(string(cfg), "[mcp_servers.") != 2 || bridge.Env["BROWSER_USE_AVAILABLE_BACKENDS"] != "chrome" {
		t.Fatalf("configuration: %s", cfg)
	}
	for _, line := range strings.Split(string(cfg), "\n") {
		key, value, ok := strings.Cut(line, " = ")
		if !ok || !strings.HasPrefix(value, `"`) {
			continue
		}
		decoded, err := tomltest.BasicString(value)
		if err != nil {
			t.Fatalf("TOML %s: %v", key, err)
		}
		if key == `"BROWSER_USE_CODEX_APP_VERSION"` && decoded != "quoted\"\n\x01version" {
			t.Fatalf("lost value: %q", decoded)
		}
	}
	for _, change := range []func(map[string]any){
		func(m map[string]any) { m["enabled"] = false },
		func(m map[string]any) { m["name"] = "other" },
		func(m map[string]any) { m["transport"].(map[string]any)["type"] = "streamable_http" },
		func(m map[string]any) { m["transport"].(map[string]any)["args"] = []string{"--unsafe"} },
		func(m map[string]any) { m["transport"].(map[string]any)["command"] = "relative" },
		func(m map[string]any) { m["transport"].(map[string]any)["env"] = map[string]string{} },
	} {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		change(m)
		mutated, _ := json.Marshal(m)
		_, err := parseCodexBrowserBridge(mutated)
		var failure *CapabilityError
		if !errors.As(err, &failure) || failure.Code != CapabilityBrowserBridgeUnavailable || failure.Phase != BeforeLaunch {
			t.Fatalf("refusal: %v", err)
		}
	}
}

func TestBrowserCanaryResultRequiresMatchingCall(t *testing.T) {
	for raw, good := range map[string]bool{`"browser-canary-denied:EPERM"`: true, `[{"type":"input_text","text":"browser-canary-denied:EPERM"}]`: true, `{"text":"browser-canary-denied:EPERM"}`: false, `[{"type":"image","text":"browser-canary-denied:EPERM"}]`: false} {
		_, ok := browserCanaryOutput(json.RawMessage(raw))
		if ok != good {
			t.Fatalf("%s: %t", raw, ok)
		}
	}
}

func TestBrowserInstallationCannotBeWorkspaceCode(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"node_repl", "node", "browser-service.mjs"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := &codexBrowserBridge{Command: filepath.Join(dir, "node_repl"), Env: map[string]string{"NODE_REPL_NODE_PATH": filepath.Join(dir, "node"), "NODE_REPL_NODE_MODULE_DIRS": dir, "NODE_REPL_TRUSTED_SERVICES": string(mustMarshal(map[string]string{"browser": filepath.Join(dir, "browser-service.mjs")}))}}
	for _, work := range []string{dir, filepath.Join(dir, "project"), filepath.Dir(dir)} {
		if b.outsideWorkspace(work) == nil {
			t.Fatalf("trusted workspace %s admitted", work)
		}
	}
	if err := b.outsideWorkspace(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserCanaryProviderOnlyCallsJSOnceAndReadsItsResult(t *testing.T) {
	result := make(chan string, 1)
	handler := browserCanaryProvider("synthetic canary", result)
	post := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(body)))
		return recorder
	}
	first := post(`{"input":[]}`)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"namespace":"mcp__node_repl"`) || !strings.Contains(first.Body.String(), `"name":"js"`) {
		t.Fatalf("call: %s", first.Body.String())
	}
	if retry := post(`{"input":[]}`); retry.Code != http.StatusServiceUnavailable {
		t.Fatal("scripted a second call")
	}
	post(`{"input":[{"type":"function_call_output","call_id":"other","output":"browser-canary-denied:EPERM"}]}`)
	select {
	case <-result:
		t.Fatal("unrelated result admitted")
	default:
	}
	post(`{"input":[{"type":"function_call_output","call_id":"harness_browser_canary","output":[{"type":"input_text","text":"browser-canary-denied:EPERM"}]}]}`)
	select {
	case text := <-result:
		if strings.TrimSpace(text) != browserCanaryDenied+"EPERM" {
			t.Fatal(text)
		}
	default:
		t.Fatal("canary result missing")
	}
}

func TestSandboxBrowserEnvironmentCannotWidenBridge(t *testing.T) {
	for _, entry := range []string{"NODE_REPL_TRUSTED_SERVICES=desktop", "BROWSER_USE_AVAILABLE_BACKENDS=iab", "SKY_CUA_SERVICE_PATH=desktop"} {
		o := Options{Provider: harness.Provider{Engine: harness.Codex}, Browser: true, Env: []string{entry}}
		if err := validateEnv(o); err != nil {
			t.Fatalf("ordinary behavior changed: %v", err)
		}
		o.Sandbox = &Sandbox{}
		var refused *UnsupportedError
		if err := validateEnv(o); !errors.As(err, &refused) || refused.Code != RefusedEnvManaged {
			t.Fatalf("%s admitted: %v", entry, err)
		}
	}
}
