//go:build !windows

package session

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestBrowserIsRefusedWhereItCannotBeHonoured(t *testing.T) {
	work := t.TempDir()
	for name, tc := range map[string]struct {
		o    Options
		code string
	}{
		"codex restricted": {Options{Provider: harness.Provider{Engine: harness.Codex}, WorkDir: work, Browser: true, Restriction: &Restriction{}}, RefusedConflict},
		"grok":             {Options{Provider: harness.Provider{Engine: harness.Grok}, WorkDir: work, Browser: true}, RefusedNotOffered},
		"restricted":       {Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Browser: true, Restriction: &Restriction{}}, RefusedConflict},
	} {
		_, err := normalize(tc.o)
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || unsupported.Code != tc.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Browser: true}); err != nil {
		t.Fatalf("an ordinary Claude session with a browser: %v", err)
	}
}

func TestNativeBrowserStartupAndResume(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
			binary, log := fakeHarness(t, fakeClean)
			o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: t.TempDir()}}, WorkDir: t.TempDir(), Browser: true}
			ctx := probeContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			runTurn(t, ctx, s, "browser start")
			ref := s.Ref()
			if _, err := s.Release(ctx); err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(ctx, o, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			runTurn(t, ctx, resumed, "browser resume")
			if _, err := resumed.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if invocations(t, log, "probe") != 0 {
				t.Fatal("ordinary browser session ran a restriction probe")
			}
			if invocations(t, log, "session") != 2 {
				t.Fatal("start and resume did not launch separately")
			}
		})
	}
}

func TestMissingBrowserFailsBeforePrompt(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		for _, scenario := range []string{"browser-missing", "browser-empty"} {
			t.Run(string(engine)+" "+scenario, func(t *testing.T) {
				binary, log := fakeHarness(t, scenario)
				o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: t.TempDir()}}, WorkDir: t.TempDir(), Browser: true}
				s, err := Start(probeContext(t), o)
				if s != nil {
					s.Close()
					t.Fatal("session opened without browser tools")
				}
				var failure *CapabilityError
				if !errors.As(err, &failure) || failure.Code != CapabilityBrowserToolsMissing || failure.Phase != BeforeFirstPrompt {
					t.Fatalf("lost capability failure: %v", err)
				}
				raw, readErr := os.ReadFile(log)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if strings.Contains(string(raw), "input:") {
					t.Fatal("missing browser sent a prompt")
				}
			})
		}
	}
}

func TestBrowserArgsAndReference(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: "claude"}}, WorkDir: t.TempDir()}
	if slices.Contains(commandArgs(o, "id", false, nil), "--chrome") {
		t.Fatal("a session without Browser enables the browser")
	}
	plain := reference(o, "id").ConfigHash
	o.Browser = true
	if !slices.Contains(commandArgs(o, "id", false, nil), "--chrome") {
		t.Fatal("Browser does not enable the browser")
	}
	if reference(o, "id").ConfigHash == plain {
		t.Fatal("a resume could add a browser without changing the reference")
	}
}

// A sandboxed session with a browser keeps MCP open only for the browser's
// server, allows exactly the admitted tools, and denies the withheld ones.
func TestSandboxedBrowserPermissions(t *testing.T) {
	o := sandboxOptions(t, harness.Claude, "claude", true)
	plain := strings.Join(claudeSandboxArgs(o), " ")
	if !strings.Contains(plain, "mcp__*") || strings.Contains(plain, claudeBrowserServer) {
		t.Fatalf("a sandbox without a browser: %s", plain)
	}
	o.Browser = true
	args := strings.Join(claudeSandboxArgs(o), " ")
	if strings.Contains(args, "mcp__*") {
		t.Fatal("MCP denied wholesale would deny the browser")
	}
	for _, tool := range claudeBrowserTools(claudeBrowserWithheld) {
		if !strings.Contains(args, tool) {
			t.Errorf("%s is not denied", tool)
		}
	}
	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
		DisableConnectors bool `json:"disableClaudeAiConnectors"`
	}
	if err := json.Unmarshal([]byte(claudeSandboxSettings(o)), &settings); err != nil {
		t.Fatal(err)
	}
	for _, tool := range claudeBrowserTools(claudeBrowserAdmitted) {
		if !slices.Contains(settings.Permissions.Allow, tool) {
			t.Errorf("%s is not allowed", tool)
		}
	}
	for _, tool := range claudeBrowserTools(claudeBrowserWithheld) {
		if slices.Contains(settings.Permissions.Allow, tool) {
			t.Errorf("%s is allowed", tool)
		}
	}
	if !settings.DisableConnectors {
		t.Fatal("claude.ai connectors stay on beside the browser")
	}
}

func TestSandboxedBrowserStartupCrossCheck(t *testing.T) {
	chrome := `{"name":"claude-in-chrome","status":"connected"}`
	crew := `{"name":"crew","status":"connected"}`
	browserTools := `"mcp__claude-in-chrome__navigate","mcp__claude-in-chrome__read_page"`
	cases := map[string]struct {
		hosted bool
		frame  string
		code   string
	}{
		"browser beside native":   {false, `{"type":"system","subtype":"init","mcp_servers":[` + chrome + `],"tools":["Bash",` + browserTools + `]}`, ""},
		"browser not loaded":      {false, `{"type":"system","subtype":"init","mcp_servers":[],"tools":["Bash"]}`, CapabilityBrowserToolsMissing},
		"browser without tools":   {false, `{"type":"system","subtype":"init","mcp_servers":[` + chrome + `],"tools":["Bash"]}`, CapabilityBrowserToolsMissing},
		"another server":          {false, `{"type":"system","subtype":"init","mcp_servers":[` + chrome + `,{"name":"claude.ai Gmail","status":"connected"}],"tools":["Bash",` + browserTools + `,"mcp__claude_ai_Gmail__send"]}`, CapabilityNativeToolsPresent},
		"browser and hosted":      {true, `{"type":"system","subtype":"init","mcp_servers":[` + chrome + `,` + crew + `],"tools":["Bash",` + browserTools + `,"mcp__crew__read_file"]}`, ""},
		"hosted server not given": {true, `{"type":"system","subtype":"init","mcp_servers":[` + chrome + `],"tools":["Bash",` + browserTools + `]}`, CapabilityServerNotLoaded},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := fakeSession(t, harness.Claude)
			s.options.Browser = true
			s.options.Sandbox = &Sandbox{Write: true}
			if c.hosted {
				s.options.Sandbox.Tools = &ToolHost{Server: "crew", Tools: []ToolDefinition{{Name: "read_file"}}}
			}
			notify(s, c.frame)
			failed := s.Health().State == Exited || s.Health().State == Failed
			if c.code == "" {
				if failed {
					t.Fatalf("closed: %v", s.failure)
				}
				return
			}
			var failure *CapabilityError
			if !failed || !errors.As(s.failure, &failure) || failure.Code != c.code || failure.Phase != BeforeFirstPrompt {
				t.Fatalf("want %s, got %v", c.code, s.failure)
			}
		})
	}
}
