//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func sandboxToolHost(t *testing.T) *ToolHost {
	t.Helper()
	return &ToolHost{
		Server:  "crew",
		Tools:   []ToolDefinition{{Name: "read_file", Schema: map[string]any{"type": "object"}}, {Name: "finish", Schema: map[string]any{"type": "object"}, Closing: true}},
		Handler: echoHandler(t), Dir: privateDir(t), Bridge: Bridge{Path: "/usr/bin/true", Args: []string{"tool-bridge"}},
	}
}

func sandboxToolOptions(t *testing.T, engine Engine, binary string, web bool) Options {
	t.Helper()
	o := sandboxOptions(t, engine, binary, true)
	o.Sandbox.Web = web
	o.Sandbox.Tools = sandboxToolHost(t)
	return o
}

func argValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			return value, true
		}
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// Hosting tools lifts only the wholesale MCP deny, which in Claude Code would
// outrank the hosted tools' allow rules. Only the caller's server is loaded,
// only its tools are allowed, and the connectors a login would fetch are off.
func TestSandboxedClaudeHostsTools(t *testing.T) {
	for _, web := range []bool{false, true} {
		o := mustNormalize(t, sandboxToolOptions(t, Claude, "/usr/bin/true", web))
		host, err := newToolHost(*o.Sandbox.Tools, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer host.close()
		extra := append(claudeSandboxArgs(o), claudeMCPConfig(host), claudeHostedAllowed(host))
		args := commandArgs(o, "id", false, &launch{host: host, extra: extra})
		if !slices.Contains(args, "--strict-mcp-config") {
			t.Fatalf("web=%v: other MCP configuration would load: %v", web, args)
		}
		disallowed, present := argValue(args, "--disallowedTools")
		if strings.Contains(disallowed, "mcp__") || present == web {
			t.Fatalf("web=%v: disallowed tools %q (present=%v)", web, disallowed, present)
		}
		allowed, _ := argValue(args, "--allowedTools")
		if allowed != "mcp__crew__read_file,mcp__crew__finish" {
			t.Fatalf("web=%v: allowed %q", web, allowed)
		}
		raw, _ := argValue(args, "--mcp-config")
		var config struct {
			MCPServers map[string]struct {
				Command string
				Args    []string
				Env     map[string]string
			}
		}
		if err = json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatal(err)
		}
		server, ok := config.MCPServers["crew"]
		if len(config.MCPServers) != 1 || !ok || server.Command != "/usr/bin/true" || server.Env[BridgeSocketEnv] != host.socket {
			t.Fatalf("web=%v: bridge not configured: %s", web, raw)
		}
		var settings struct {
			DisableClaudeAiConnectors bool
			Permissions               struct{ Allow, Deny []string }
		}
		doc, _ := argValue(args, "--settings")
		if err = json.Unmarshal([]byte(doc), &settings); err != nil {
			t.Fatal(err)
		}
		if !settings.DisableClaudeAiConnectors || !slices.Contains(settings.Permissions.Allow, "mcp__crew__read_file") || !slices.Contains(settings.Permissions.Allow, "mcp__crew__finish") {
			t.Fatalf("web=%v: settings do not confine MCP to the hosted tools: %s", web, doc)
		}
		if slices.ContainsFunc(settings.Permissions.Allow, func(rule string) bool { return strings.HasSuffix(rule, "*") && strings.HasPrefix(rule, "mcp__") }) {
			t.Fatalf("web=%v: a wildcard MCP allow rule: %v", web, settings.Permissions.Allow)
		}
	}
	plain := mustNormalize(t, Options{Engine: Claude, WorkDir: t.TempDir(), Sandbox: &Sandbox{Write: true}})
	if strings.Contains(claudeSandboxSettings(plain), "disableClaudeAiConnectors") {
		t.Fatal("a sandbox without tools changed its settings")
	}
}

func TestSandboxedCodexHostsTools(t *testing.T) {
	o := mustNormalize(t, sandboxToolOptions(t, Codex, "/usr/bin/true", false))
	host, err := newToolHost(*o.Sandbox.Tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	server, err := codexHostedServer(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`mcp_servers.crew.command="/usr/bin/true"`, `mcp_servers.crew.args=["tool-bridge"]`, `mcp_servers.crew.default_tools_approval_mode="approve"`, "mcp_servers.crew.env." + BridgeSocketEnv + "="} {
		if !slices.ContainsFunc(server, func(setting string) bool { return strings.HasPrefix(setting, want) }) {
			t.Errorf("missing %s in %v", want, server)
		}
	}
	for _, setting := range server {
		if !strings.HasPrefix(setting, "mcp_servers.crew.") {
			t.Errorf("hosting tools changed more than its own server: %s", setting)
		}
	}
}

func TestSandboxToolsAreRefusedWhenTheyCannotBeHosted(t *testing.T) {
	work := t.TempDir()
	cases := map[string]func(*ToolHost){
		"no handler":           func(h *ToolHost) { h.Handler = nil },
		"invalid server":       func(h *ToolHost) { h.Server = "not a name" },
		"reserved server":      func(h *ToolHost) { h.Server = "workspace" },
		"inside the workspace": func(h *ToolHost) { h.Dir = work + "/.crew" },
		"the workspace itself": func(h *ToolHost) { h.Dir = work },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			host := sandboxToolHost(t)
			edit(host)
			if _, err := normalize(Options{Engine: Claude, WorkDir: work, Sandbox: &Sandbox{Write: true, Tools: host}}); err == nil {
				t.Fatal("tool host accepted")
			}
		})
	}
	host := sandboxToolHost(t)
	caller := host.Tools[0].Schema
	o := mustNormalize(t, Options{Engine: Claude, WorkDir: work, Sandbox: &Sandbox{Tools: host}})
	o.Sandbox.Tools.Tools[0].Schema["edited"] = true
	o.Sandbox.Tools.Server = "other"
	if _, edited := caller["edited"]; edited || host.Server != "crew" {
		t.Fatal("normalizing shared the caller's tool host")
	}
}

// The server's name is part of what a sandboxed conversation is, as it is for
// a restricted one; the channel material and the tool list are not.
func TestSandboxToolServerIsPartOfTheReference(t *testing.T) {
	work, home := t.TempDir(), t.TempDir()
	hash := func(tools *ToolHost) string {
		return reference(mustNormalize(t, Options{Engine: Claude, WorkDir: work, Home: home, Sandbox: &Sandbox{Write: true, Tools: tools}}), "id").ConfigHash
	}
	first, second, renamed := sandboxToolHost(t), sandboxToolHost(t), sandboxToolHost(t)
	second.Tools = second.Tools[:1]
	renamed.Server = "other"
	if hash(first) != hash(second) {
		t.Fatal("a new channel or tool list orphaned the conversation")
	}
	if hash(first) == hash(renamed) || hash(first) == hash(nil) {
		t.Fatal("a resume could change the tool server without changing the reference")
	}
}

// A sandboxed Claude session keeps its own tools, so its startup report is
// judged on MCP alone: the hosted tools must be there and no other server's.
func TestSandboxedClaudeStartupCrossCheck(t *testing.T) {
	cases := map[string]struct {
		frame string
		code  string
	}{
		"hosted beside native": {`{"type":"system","subtype":"init","mcp_servers":[{"name":"crew","status":"connected"}],"tools":["Bash","Read","WebFetch","mcp__crew__read_file","mcp__crew__finish"]}`, ""},
		"another server":       {`{"type":"system","subtype":"init","mcp_servers":[{"name":"crew","status":"connected"},{"name":"claude.ai Gmail","status":"connected"}],"tools":["Bash","mcp__crew__read_file","mcp__crew__finish","mcp__claude_ai_Gmail__send"]}`, CapabilityNativeToolsPresent},
		"a hosted tool absent": {`{"type":"system","subtype":"init","mcp_servers":[{"name":"crew","status":"connected"}],"tools":["Bash","mcp__crew__read_file"]}`, CapabilityHostedToolsMissing},
		"server not loaded":    {`{"type":"system","subtype":"init","mcp_servers":[],"tools":["Bash","Read"]}`, CapabilityServerNotLoaded},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := fakeSession(t, Claude)
			s.options.Sandbox = &Sandbox{Write: true, Tools: &ToolHost{Server: "crew", Tools: []ToolDefinition{{Name: "read_file"}, {Name: "finish"}}}}
			notify(s, c.frame)
			failed := s.Health().State == Exited || s.Health().State == Failed
			if c.code == "" {
				if failed {
					t.Fatalf("a sandboxed surface with its hosted tools closed the session: %v", s.failure)
				}
			} else {
				var failure *CapabilityError
				if !failed || !errors.As(s.failure, &failure) || failure.Code != c.code || failure.Phase != BeforeFirstPrompt {
					t.Fatalf("want %s, got %v", c.code, s.failure)
				}
			}
			if s.Capabilities().RestrictTools.Availability != CapabilitiesFor(Claude).RestrictTools.Availability {
				t.Fatal("a sandboxed session reported itself restricted")
			}
		})
	}
}

// End to end through the fake CLIs: the sandbox is proved, the tool channel
// opens under the assignment lease with a launch record, the harness is
// launched with both the sandbox and the bridge, and Release settles it all.
func TestSandboxedSessionServesItsTools(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
			binary, log := fakeHarness(t, fakeSandboxOK)
			o := sandboxToolOptions(t, engine, binary, true)
			ctx := probeContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = newToolHost(*o.Sandbox.Tools, nil); !errors.Is(err, ErrLeaseHeld) {
				t.Fatalf("the running session does not hold its assignment lease: %v", err)
			}
			record, err := readLaunchRecord(o.Sandbox.Tools.Dir)
			if err != nil || record == nil || !record.identified() {
				t.Fatalf("the harness was not recorded: %+v %v", record, err)
			}
			runTurn(t, ctx, s, "hello")

			launches := logged(t, log, "args:")
			if len(launches) != 1 {
				t.Fatalf("want one credentialed launch, got %d", len(launches))
			}
			required := []string{`"--strict-mcp-config"`, `--mcp-config=`, `"--allowedTools=mcp__crew__read_file,mcp__crew__finish"`, `"--tools=Bash,Read,Edit,Write,Glob,Grep,WebFetch,WebSearch"`, `disableClaudeAiConnectors`}
			check := "status"
			if engine == Codex {
				required = []string{`default_permissions=\"harness_sandbox\"`, `web_search=\"live\"`, `mcp_servers.crew.command=`, `mcp_servers.crew.default_tools_approval_mode=\"approve\"`}
				check = "canary"
			}
			for _, want := range required {
				if !strings.Contains(launches[0], want) {
					t.Errorf("launch is missing %s in %s", want, launches[0])
				}
			}
			if invocations(t, log, check) != 1 {
				t.Fatal("the sandbox was not proved exactly once before launch")
			}

			secret, err := os.ReadFile(secretPath(o.Sandbox.Tools.Dir))
			if err != nil {
				t.Fatal(err)
			}
			c := dial(t, s.tools, string(secret))
			c.send(t, "tools/list", map[string]any{})
			if listed, _ := json.Marshal(c.receive(t)); !strings.Contains(string(listed), `"read_file"`) {
				t.Fatalf("the channel did not serve the hosted tools: %s", listed)
			}

			release(t, ctx, s)
			if record, err = readLaunchRecord(o.Sandbox.Tools.Dir); err != nil || record != nil {
				t.Fatalf("release left the launch record: %+v %v", record, err)
			}
		})
	}
}

// The sandbox evidence is about the sandbox. A per-launch channel path in the
// launch arguments must not make every launch re-prove it.
func TestSandboxEvidenceSurvivesANewToolChannel(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxToolOptions(t, Codex, binary, false)
	ctx := probeContext(t)
	for range 2 {
		s, err := Start(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		release(t, ctx, s)
	}
	if invocations(t, log, "canary") != 1 || invocations(t, log, "session") != 2 {
		t.Fatal("proved sandbox evidence was not reused across tool channels")
	}
}

// A sandbox that fails its check opens no tool channel and keeps no lease.
func TestFailedSandboxLeavesNoToolChannel(t *testing.T) {
	binary, log := fakeHarness(t, fakeCanaryNetwork)
	o := sandboxToolOptions(t, Codex, binary, false)
	s, _, err := Open(probeContext(t), o, nil)
	if s != nil {
		s.Close()
		t.Fatal("session started")
	}
	if capabilityCode(t, err) != CapabilitySandboxNotEnforced || invocations(t, log, "session") != 0 {
		t.Fatalf("a credentialed session launched after a failed canary: %v", err)
	}
	host, err := newToolHost(*o.Sandbox.Tools, nil)
	if err != nil {
		t.Fatalf("the refused launch kept its lease: %v", err)
	}
	host.close()
	if record, err := readLaunchRecord(o.Sandbox.Tools.Dir); err != nil || record != nil {
		t.Fatalf("a refused launch left a record: %+v %v", record, err)
	}
}

func TestSandboxedToolsVerifyWithoutAChannel(t *testing.T) {
	binary, _ := fakeHarness(t, fakeSandboxOK)
	o := sandboxToolOptions(t, Claude, binary, false)
	if err := VerifySandbox(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secretPath(o.Sandbox.Tools.Dir)); !os.IsNotExist(err) {
		t.Fatal("verifying a sandbox opened its tool channel")
	}
}
