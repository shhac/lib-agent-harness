package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
)

// claudeBrowserServer is the MCP server Claude Code 2.1.283 adds for --chrome.
// It is a dynamic server, so --strict-mcp-config keeps it.
const claudeBrowserServer = "claude-in-chrome"

// claudeBrowserWithheld are the Claude in Chrome tools a sandboxed session
// never gets: file_upload reads local files from outside the sandbox, and a
// shortcut starts another agent in the browser's side panel.
var claudeBrowserWithheld = []string{"file_upload", "shortcuts_execute", "shortcuts_list"}

// claudeBrowserAdmitted are the Claude in Chrome tools, as Claude Code 2.1.283
// lists them, that a sandboxed session may call. A tool a later build adds has
// no allow rule, so dontAsk refuses it until it is reviewed here.
var claudeBrowserAdmitted = []string{
	"browser_batch", "computer", "find", "form_input", "get_page_text", "gif_creator",
	"javascript_tool", "list_connected_browsers", "navigate", "read_console_messages",
	"read_network_requests", "read_page", "resize_window", "select_browser",
	"switch_browser", "tabs_close_mcp", "tabs_context_mcp", "tabs_create_mcp", "upload_image",
}

func claudeBrowserTools(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "mcp__"+claudeBrowserServer+"__"+name)
	}
	return out
}

// normalizeBrowser refuses a browser the engine does not ship, and one asked
// of a restricted session, whose tools are exactly the caller's.
func normalizeBrowser(o Options) error {
	if !o.Browser {
		return nil
	}
	if c := harness.Support(o.Provider.Engine, harness.Session, harness.Browser); !c.Usable() {
		return &UnsupportedError{Engine: o.Provider.Engine, Operation: "browser", Code: RefusedNotOffered, Capability: c}
	}
	if o.Restriction != nil {
		return refuse(o, "browser", RefusedConflict, "a restricted session's tools are exactly the caller's; leave Browser unset or use an ordinary native session")
	}
	if o.Sandbox != nil {
		if c := harness.Support(o.Provider.Engine, harness.Session, harness.SandboxedBrowser); !c.Usable() {
			return refuse(o, "browser", RefusedConflict, c.Reason+"; leave Browser unset or use an ordinary native session")
		}
	}
	return nil
}

// normalizeBackground refuses background priority where it is not offered.
func normalizeBackground(o Options) error {
	if !o.Background {
		return nil
	}
	if o.Provider.Engine == harness.OpenAICompatible && (o.Workbench == nil || o.Workbench.Commands == nil) {
		return &UnsupportedError{Engine: o.Provider.Engine, Operation: "background", Code: RefusedNotOffered, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "API background priority requires Workbench.Commands"}}
	}
	if c := harness.Support(o.Provider.Engine, harness.Session, harness.Background); !c.Usable() {
		return &UnsupportedError{Engine: o.Provider.Engine, Operation: "background", Code: RefusedNotOffered, Capability: c}
	}
	return nil
}

// browserTool reports whether an advertised tool is the browser's. Which of
// them a sandboxed session may call is decided by its permission rules.
func browserTool(o Options, name string) bool {
	return o.Browser && strings.HasPrefix(name, "mcp__"+claudeBrowserServer+"__")
}

// checkCodexBrowser checks the configured bridge's advertised tools, including
// deferred ones. This does not execute JavaScript, open a browser, or prove an
// extension connection. The protocol and identities were checked against
// codex-cli 0.159.2 with a local provider that refuses inference.
func checkCodexBrowser(ctx context.Context, w wire, threadID string) error {
	missing := &CapabilityError{Engine: harness.Codex, Code: CapabilityBrowserToolsMissing, Phase: BeforeFirstPrompt}
	body, err := w.request(ctx, "mcpServerStatus/list", map[string]any{"threadId": threadID, "serverName": "node_repl", "detail": "toolsAndAuthOnly"})
	if err != nil {
		if errors.Is(err, ErrRejected) {
			return missing
		}
		return err
	}
	var status struct {
		Data []struct {
			Name          string  `json:"name"`
			RuntimeStatus string  `json:"runtimeStatus"`
			ToolsError    *string `json:"toolsError"`
			Tools         map[string]struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &status) != nil || status.Data == nil {
		return &CapabilityError{Engine: harness.Codex, Code: CapabilityProbeUnreadable, Phase: BeforeFirstPrompt}
	}
	for _, server := range status.Data {
		if server.Name == "node_repl" && server.ToolsError == nil &&
			(server.RuntimeStatus == "" || server.RuntimeStatus == "connected") &&
			server.Tools["js"].Name == "js" && server.Tools["js_reset"].Name == "js_reset" {
			return nil
		}
	}
	return missing
}
