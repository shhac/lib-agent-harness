package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func restrictedOptions(t *testing.T, engine Engine) Options {
	t.Helper()
	return Options{
		Engine: engine, Binary: "/usr/bin/true", WorkDir: t.TempDir(), Home: t.TempDir(), Model: "picked",
		Restriction: &Restriction{Tools: ToolHost{
			Server:  "workspace",
			Tools:   []ToolDefinition{{Name: "read_file", Schema: map[string]any{"type": "object"}}, {Name: "finish", Schema: map[string]any{"type": "object"}, Closing: true}},
			Handler: echoHandler(t), Dir: privateDir(t), Bridge: Bridge{Path: "/usr/bin/true", Args: []string{"tool-bridge"}},
		}},
	}
}

// The reference has to survive a restart. The listener path and the channel
// credential are new every launch; if they contributed, every stored session
// reference would be unresumable after the owning process restarted.
func TestReferenceIgnoresEphemeralChannelMaterialButNotTheToolSurface(t *testing.T) {
	first := restrictedOptions(t, Claude)
	first, err := normalize(first)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Restriction = &Restriction{Tools: first.Restriction.Tools}
	second.Restriction.Tools.Dir = privateDir(t)
	second.Restriction.Tools.Bridge.Args = []string{"tool-bridge"}
	if reference(first, "s1") != reference(second, "s1") {
		t.Fatal("a new private channel directory invalidated the session reference")
	}
	changed := first
	changed.Restriction = &Restriction{Tools: first.Restriction.Tools}
	changed.Restriction.Tools.Tools = append(append([]ToolDefinition(nil), first.Restriction.Tools.Tools...), ToolDefinition{Name: "run_command", Schema: map[string]any{"type": "object"}})
	if reference(first, "s1") == reference(changed, "s1") {
		t.Fatal("adding a tool did not change the session reference")
	}
	// Order is a caller detail, not a contract change.
	reordered := first
	reordered.Restriction = &Restriction{Tools: first.Restriction.Tools}
	reordered.Restriction.Tools.Tools = []ToolDefinition{first.Restriction.Tools.Tools[1], first.Restriction.Tools.Tools[0]}
	if reference(first, "s1") != reference(reordered, "s1") {
		t.Fatal("tool ordering changed the session reference")
	}
	// An unrestricted session's reference must be unchanged by this work.
	plain := Options{Engine: Claude, Binary: "claude", WorkDir: first.WorkDir, Home: first.Home}
	plain, err = normalize(plain)
	if err != nil {
		t.Fatal(err)
	}
	if reference(plain, "s1") == reference(first, "s1") {
		t.Fatal("a restricted session shares a reference with an unrestricted one")
	}
}

func TestRestrictedClaudeArgumentsDisableInheritedSurfaces(t *testing.T) {
	o, err := normalize(restrictedOptions(t, Claude))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newToolHost(o.Restriction.Tools)
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	args := strings.Join(commandArgs(o, "session-1", false, &launch{host: host, extra: claudeRestrictedArgs(host)}), "\n")
	for _, required := range []string{"--setting-sources=", `--settings={"disableAllHooks":true}`, "--strict-mcp-config", "--disable-slash-commands", "--no-chrome", "--tools=mcp__workspace__read_file,mcp__workspace__finish"} {
		if !strings.Contains(args, required) {
			t.Errorf("missing %q in\n%s", required, args)
		}
	}
	if strings.Contains(args, "bypassPermissions") || strings.Contains(args, "dangerously") {
		t.Error("restricted arguments named a permission bypass")
	}
	config := ""
	for _, arg := range strings.Split(args, "\n") {
		if strings.HasPrefix(arg, "--mcp-config=") {
			config = strings.TrimPrefix(arg, "--mcp-config=")
		}
	}
	var parsed struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(config), &parsed) != nil || len(parsed.Servers) != 1 {
		t.Fatalf("exactly one MCP server must be configured: %s", config)
	}
	server := parsed.Servers["workspace"]
	if server.Command != "/usr/bin/true" || len(server.Args) != 1 {
		t.Fatalf("bridge command was not configured: %+v", server)
	}
	// Paths may travel in the launch configuration; the credential may not.
	if strings.Contains(config, string(host.secret)) || strings.Contains(args, string(host.secret)) {
		t.Fatal("the channel credential reached the harness command line")
	}
	if server.Env[BridgeSocketEnv] != host.socket || server.Env[BridgeLockEnv] != filepath.Join(host.cfg.Dir, "bridge.lock") {
		t.Errorf("channel was not named for the bridge: %+v", server.Env)
	}
}

func TestRestrictedCodexArgumentsRemoveNativeToolSurfaces(t *testing.T) {
	o, err := normalize(restrictedOptions(t, Codex))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newToolHost(o.Restriction.Tools)
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	args := strings.Join(commandArgs(o, "thread", false, &launch{host: host, extra: codexRestrictedArgs(host, "/tmp/catalog.json")}), "\n")
	for _, required := range []string{"--ignore-user-config", "--ignore-rules", `model_catalog_json="/tmp/catalog.json"`, "features.shell_tool=false", "features.unified_exec=false", "features.hooks=false", "project_doc_max_bytes=0", `approval_policy="never"`, "mcp_servers.workspace="} {
		if !strings.Contains(args, required) {
			t.Errorf("missing %q in\n%s", required, args)
		}
	}
	if strings.Contains(args, string(host.secret)) {
		t.Fatal("the channel credential reached the harness command line")
	}
}

// A restricted session must never be launched with a tool surface it did not
// configure, and must never be launched missing the tools it needs.
func TestProbeJudgesTheOutboundToolSurface(t *testing.T) {
	o, err := normalize(restrictedOptions(t, Claude))
	if err != nil {
		t.Fatal(err)
	}
	request := func(names ...string) []byte {
		tools := []map[string]any{}
		for _, name := range names {
			tools = append(tools, map[string]any{"name": name})
		}
		raw, _ := json.Marshal(map[string]any{"model": "picked", "tools": tools})
		return raw
	}
	if failure := inspectProbeRequest(o, request("mcp__workspace__read_file", "mcp__workspace__finish")); failure != nil {
		t.Fatalf("exactly the hosted tools was rejected: %v", failure)
	}
	failure := inspectProbeRequest(o, request("mcp__workspace__read_file", "mcp__workspace__finish", "Bash"))
	if failure == nil || failure.Code != CapabilityNativeToolsPresent || failure.Tools[0] != "Bash" {
		t.Fatalf("a retained native tool was not rejected: %v", failure)
	}
	failure = inspectProbeRequest(o, request("mcp__workspace__read_file"))
	if failure == nil || failure.Code != CapabilityHostedToolsMissing || failure.Tools[0] != "finish" {
		t.Fatalf("a missing hosted tool was not rejected: %v", failure)
	}
	failure = inspectProbeRequest(o, []byte("not json"))
	if failure == nil || failure.Code != CapabilityProbeUnreadable {
		t.Fatalf("an unreadable request was not rejected: %v", failure)
	}
	if !errors.Is(failure, ErrUnsupported) {
		t.Error("a capability failure must be recognizable as an unsupported operation")
	}
	if !strings.Contains(failure.Error(), "no session was started") {
		t.Errorf("capability failure does not say nothing was launched: %s", failure)
	}
}

func TestProbeChecksCodexIdentityAndInheritedInstructions(t *testing.T) {
	o, err := normalize(restrictedOptions(t, Codex))
	if err != nil {
		t.Fatal(err)
	}
	o.Effort = "high"
	body := func(model, effort, extra string) []byte {
		raw, _ := json.Marshal(map[string]any{
			"model": model, "reasoning": map[string]any{"effort": effort},
			"tools": []map[string]any{{"name": "workspace__read_file"}, {"name": "workspace__finish"}},
			"input": extra,
		})
		return raw
	}
	if failure := inspectProbeRequest(o, body("picked", "high", "")); failure != nil {
		t.Fatalf("a correct Codex request was rejected: %v", failure)
	}
	if failure := inspectProbeRequest(o, body("other", "high", "")); failure == nil || failure.Code != CapabilityChangedModel {
		t.Fatalf("a substituted model was not rejected: %v", failure)
	}
	if failure := inspectProbeRequest(o, body("picked", "low", "")); failure == nil || failure.Code != CapabilityChangedEffort {
		t.Fatalf("a changed effort was not rejected: %v", failure)
	}
	if failure := inspectProbeRequest(o, body("picked", "high", "# AGENTS.md instructions")); failure == nil || failure.Code != CapabilityInstructionsMerged {
		t.Fatalf("merged global instructions were not rejected: %v", failure)
	}
}

func TestNormalizeRejectsContradictoryRestrictedConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*Options){
		"second tool policy": func(o *Options) { o.Policy.ClaudeTools = []string{} },
		"replaced prompt": func(o *Options) {
			o.Instructions = Instructions{Replace, "you are a JSON action engine"}
		},
		"no model for codex": func(o *Options) { o.Engine, o.Model = Codex, "" },
		"invalid tool host":  func(o *Options) { o.Restriction.Tools.Handler = nil },
	} {
		t.Run(name, func(t *testing.T) {
			o := restrictedOptions(t, Claude)
			mutate(&o)
			if _, err := normalize(o); err == nil {
				t.Fatal("a contradictory restricted configuration was accepted")
			}
		})
	}
	// Appending scoped instructions is the supported way to add a task.
	o := restrictedOptions(t, Claude)
	o.Instructions = Instructions{Append, "implement the assignment"}
	if _, err := normalize(o); err != nil {
		t.Fatalf("appended scoped instructions were rejected: %v", err)
	}
}

// A failed probe must leave nothing running and nothing listening.
func TestFailedPreparationReleasesTheToolChannel(t *testing.T) {
	o, err := normalize(restrictedOptions(t, Codex))
	if err != nil {
		t.Fatal(err)
	}
	o.Binary = filepath.Join(t.TempDir(), "absent-binary")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := prepareLaunch(ctx, o)
	if l != nil || err == nil {
		t.Fatalf("a session was prepared without a usable harness: %v %v", l, err)
	}
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityCatalogUnavailable {
		t.Fatalf("unexpected failure: %v", err)
	}
	entries, err := os.ReadDir(o.Restriction.Tools.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sock") {
			t.Fatal("the tool channel was left listening after preparation failed")
		}
	}
}

func TestNormalizedCatalogKeepsTheHarnessCodingInstructions(t *testing.T) {
	catalog := []byte(`{"models":[{"slug":"picked","supported_reasoning_levels":[{"effort":"high"}],"base_instructions":"native coding instructions","shell_type":"local"}]}`)
	raw, err := restrictedCatalogFor(catalog, "picked", "high")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "native coding instructions") {
		t.Error("a restricted session replaced the harness's coding instructions")
	}
	if !strings.Contains(string(raw), `"shell_type":"disabled"`) {
		t.Error("the native shell surface was not removed")
	}
	if _, err = restrictedCatalogFor(nil, "picked", "high"); err == nil {
		t.Error("an absent catalog was accepted")
	}
	_, err = restrictedCatalogFor(catalog, "absent", "high")
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityCatalogRestriction || len(failure.Tools) != 1 {
		t.Fatalf("an unknown model did not produce an actionable reason: %v", err)
	}
}

func TestNormalizeWireToolStripsEngineSpecificPrefixes(t *testing.T) {
	for wire, want := range map[string]string{
		"mcp__workspace__read_file": "read_file",
		"workspace__read_file":      "read_file",
		"workspace.read_file":       "read_file",
		"Bash":                      "Bash",
		"mcp__other__read_file":     "mcp__other__read_file",
	} {
		if got := normalizeWireTool(wire, "workspace"); got != want {
			t.Errorf("normalizeWireTool(%q) = %q, want %q", wire, got, want)
		}
	}
}
