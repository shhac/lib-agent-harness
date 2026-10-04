//go:build !windows

package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// Synthetic installation and configuration; never reads the real app or home.
func fakeBrowserBridge(t *testing.T, o Options) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"node_repl", "node", "browser-service.mjs"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"NODE_REPL_NODE_PATH": filepath.Join(dir, "node"), "NODE_REPL_NODE_MODULE_DIRS": dir,
		"NODE_REPL_TRUSTED_SERVICES":   string(mustMarshal(map[string]string{"browser": filepath.Join(dir, "browser-service.mjs"), "sky": "@oai/sky/service", "other": "secret"})),
		"NODE_REPL_TRUSTED_CODE_PATHS": o.Provider.CLI.Home, "CODEX_HOME": o.Provider.CLI.Home,
		"BROWSER_USE_AVAILABLE_BACKENDS": "chrome,iab,mcpapps", "SKY_CUA_SERVICE_PATH": "desktop", "NODE_OPTIONS": "unsafe", "SECRET_TOKEN": "secret",
	}
	raw := mustMarshal(map[string]any{"name": "node_repl", "enabled": true, "transport": map[string]any{"type": "stdio", "command": filepath.Join(dir, "node_repl"), "env": env, "env_vars": []string{"SECRET_TOKEN"}, "cwd": o.Provider.CLI.Home}})
	if err := os.WriteFile(filepath.Join(browserBridgeHome(o), "fake-browser-bridge.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func fakeBrowserProof(url, scenario string) int {
	logInvocation("browser-proof")
	if scenario == "browser-proof-crash" {
		return 2
	}
	response, err := http.Post(url, "application/json", bytes.NewReader([]byte(`{"input":[]}`)))
	if err != nil {
		return 2
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return 2
	}
	// Require a real streamed js call before synthesizing a tool result.
	var code string
	for _, line := range strings.Split(string(raw), "\n") {
		value, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type string
			Item struct {
				Name      string
				Namespace string
				Arguments string
			}
		}
		if json.Unmarshal([]byte(value), &event) != nil {
			return 2
		}
		if event.Type == "response.output_item.done" && event.Item.Name == "js" && event.Item.Namespace == "mcp__node_repl" {
			var args struct{ Code string }
			if json.Unmarshal([]byte(event.Item.Arguments), &args) != nil {
				return 2
			}
			code = args.Code
		}
	}
	if code == "" {
		return 2
	}
	if scenario == "browser-proof-no-result" {
		return 2
	}
	output := browserCanaryDenied + "EPERM"
	if scenario == "browser-proof-write" || scenario == "browser-proof-file" {
		target := regexp.MustCompile(`writeFile\(("(?:[^"\\]|\\.)*")`).FindStringSubmatch(code)
		if len(target) != 2 {
			return 2
		}
		var path string
		_ = json.Unmarshal([]byte(target[1]), &path)
		if os.WriteFile(path, []byte("x"), 0600) != nil {
			return 2
		}
		if scenario == "browser-proof-write" {
			output = browserCanaryWrote
		}
	}
	if scenario == "browser-proof-error" {
		output = "bridge could not run: secret diagnostic"
	}
	body := mustMarshal(map[string]any{"input": []any{map[string]any{"type": "function_call_output", "call_id": browserCanaryID, "output": []any{map[string]any{"type": "input_text", "text": output}}}}})
	return fakePost(url, body)
}

func TestCodexSandboxBrowserProofAdmission(t *testing.T) {
	testenv.RequireLoopback(t) // Native canary proof uses a local provider and network witnesses.
	for scenario, want := range map[string]string{"sandbox-ok": "", "browser-proof-write": CapabilityBrowserSandboxNotEnforced, "browser-proof-file": CapabilityBrowserSandboxNotEnforced, "browser-proof-error": CapabilityBrowserSandboxUnproven, "browser-proof-crash": CapabilityBrowserSandboxUnproven, "browser-proof-no-result": CapabilityBrowserSandboxUnproven} {
		t.Run(scenario, func(t *testing.T) {
			binary, log := fakeHarness(t, scenario)
			o := sandboxOptions(t, "codex", binary, true)
			o.Browser = true
			fakeBrowserBridge(t, o)
			s, err := Start(probeContext(t), o)
			if want != "" {
				if s != nil {
					s.Close()
					t.Fatal("unproven session launched")
				}
				var failure *CapabilityError
				if !errors.As(err, &failure) || failure.Code != want || failure.Phase != BeforeLaunch || !errors.Is(err, ErrUnsupported) {
					t.Fatalf("failure: %v", err)
				}
				if invocations(t, log, "session") != 0 {
					t.Fatal("credentialed launch before proof")
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("provider text leaked")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := probeContext(t)
			runTurn(t, ctx, s, "browser")
			ref := s.Ref()
			if _, err = s.Release(ctx); err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(ctx, o, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			if invocations(t, log, "browser-proof") != 1 {
				t.Fatal("proof not cached for equivalent resume")
			}
			without := o
			without.Browser = false
			if _, err = Resume(ctx, without, ref); !errors.Is(err, ErrIncompatibleResume) {
				t.Fatalf("browser removed on resume: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(o.RuntimeHome, codexConfigFile))
			if err != nil {
				t.Fatal(err)
			}
			cfg := string(raw)
			for _, forbidden := range []string{"sky", "desktop", "secret", "iab", "mcpapps", o.Provider.CLI.Home, "NODE_OPTIONS"} {
				if strings.Contains(cfg, forbidden) {
					t.Fatalf("inherited %q", forbidden)
				}
			}
			if strings.Count(cfg, "[mcp_servers.") != 2 || !strings.Contains(cfg, `"BROWSER_USE_AVAILABLE_BACKENDS" = "chrome"`) {
				t.Fatalf("runtime config: %s", cfg)
			}
			// Removing Browser must overwrite the declaration, including on a fresh turn.
			if _, err = resumed.Release(ctx); err != nil {
				t.Fatal(err)
			}
			plain, err := Start(ctx, without)
			if err != nil {
				t.Fatal(err)
			}
			defer plain.Close()
			raw, _ = os.ReadFile(filepath.Join(o.RuntimeHome, codexConfigFile))
			if string(raw) != runtimeConfig {
				t.Fatal("bridge survived Browser unset")
			}
		})
	}
}

func TestBrowserSandboxJudgement(t *testing.T) {
	missing := &os.PathError{Op: "lstat", Path: "synthetic", Err: os.ErrNotExist}
	for name, tc := range map[string]struct {
		output string
		stat   error
		code   string
	}{
		"EPERM": {browserCanaryDenied + "EPERM", missing, ""}, "EACCES": {browserCanaryDenied + "EACCES", missing, ""},
		"nothing ran": {"", missing, CapabilityBrowserSandboxUnproven}, "wrong denial": {browserCanaryDenied + "ENOENT", missing, CapabilityBrowserSandboxUnproven},
		"wrote then removed": {browserCanaryWrote, missing, CapabilityBrowserSandboxNotEnforced}, "file exists": {browserCanaryDenied + "EPERM", nil, CapabilityBrowserSandboxNotEnforced},
		"stat failed": {browserCanaryDenied + "EPERM", os.ErrPermission, CapabilityBrowserSandboxUnproven},
	} {
		t.Run(name, func(t *testing.T) {
			err := judgeBrowserSandbox(tc.output, tc.stat)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if capabilityCode(t, err) != tc.code {
				t.Fatal(err)
			}
		})
	}
}

func TestSandboxBrowserEvidenceChangesWithInstallation(t *testing.T) {
	testenv.RequireLoopback(t) // Native canary proof uses a local provider and network witnesses.
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, "codex", binary, true)
	o.Browser = true
	fakeBrowserBridge(t, o)
	ctx := probeContext(t)
	for range 2 {
		if err := VerifySandbox(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if invocations(t, log, "browser-proof") != 1 {
		t.Fatal("equivalent verification not cached")
	}
	b, err := readCodexBrowserBridge(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(b.Command, []byte("upgraded installation"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = VerifySandbox(ctx, o); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "browser-proof") != 2 {
		t.Fatal("upgraded bridge reused old proof")
	}
	o.Sandbox = &Sandbox{Write: false}
	if err = VerifySandbox(ctx, o); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "browser-proof") != 3 {
		t.Fatal("different sandbox reused old proof")
	}
	o.Browser = false
	if err = VerifySandbox(ctx, o); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "browser-proof") != 3 {
		t.Fatal("Browser unset ran a browser proof")
	}
}

func TestSandboxBrowserHostCannotReplaceBridge(t *testing.T) {
	o := sandboxOptions(t, "codex", "codex", true)
	o.Browser = true
	o.Sandbox.Tools = sandboxToolConfig(t)
	o.Sandbox.Tools.Server = "node_repl"
	_, err := normalize(o)
	if capabilityCode(t, err) != CapabilityServerNameReserved {
		t.Fatal(err)
	}
}
