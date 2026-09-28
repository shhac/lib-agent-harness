package session

import (
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
		return refuse(o, "browser", RefusedConflict, "a restricted session's tools are exactly the caller's; leave Browser unset or use a sandboxed session")
	}
	return nil
}

// browserTool reports whether an advertised tool is the browser's. Which of
// them a sandboxed session may call is decided by its permission rules.
func browserTool(o Options, name string) bool {
	return o.Browser && strings.HasPrefix(name, "mcp__"+claudeBrowserServer+"__")
}
