package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// claudeSandboxSettings is the settings document a sandboxed Claude session
// and its status check both load, with every other settings source dropped.
func claudeSandboxSettings(o Options) string {
	var allow, deny []string
	if !o.Sandbox.Web {
		deny = append(deny, claudeWebTools...)
	}
	filesystem := map[string]any{}
	if o.Sandbox.Write {
		// Permission rules take "//" for an absolute path. Repository metadata
		// stays read-only, as in the Codex profile: hooks or config written
		// there would run wherever git next runs, outside this sandbox.
		gitDir := "/" + filepath.Join(o.WorkDir, ".git") + "/**"
		allow = append(allow, "Edit(/"+o.WorkDir+"/**)")
		deny = append(deny, "Edit("+gitDir+")", "Write("+gitDir+")")
		filesystem["denyWrite"] = []string{filepath.Join(o.WorkDir, ".git")}
	} else {
		deny = append(deny, "Edit", "Write")
		// Sandbox paths are plain absolute paths. The shell may otherwise write
		// to its working directory by default.
		filesystem["denyWrite"] = []string{o.WorkDir}
	}
	if reads := append(slices.Clone(o.Sandbox.Read), sandboxSkillReads(o)...); len(reads) > 0 {
		filesystem["allowRead"] = reads
		for _, dir := range reads {
			allow = append(allow, "Read(/"+dir+"/**)")
		}
	}
	if o.Sandbox.Web {
		// Bare tool names, never WebFetch(domain:...): Claude Code adds every
		// domain such a rule names to the shell's network allowlist too.
		// dontAsk refuses a tool with no allow rule, so these are required.
		allow = append(allow, claudeWebTools...)
	}
	if o.Sandbox.Tools != nil {
		allow = append(allow, o.Sandbox.Tools.Qualified()...)
	}
	if o.Browser {
		allow = append(allow, claudeBrowserTools(claudeBrowserAdmitted)...)
	}
	permissions := map[string]any{
		"defaultMode":                         "dontAsk",
		"deny":                                deny,
		"blockReadsOutsideWorkingDirectories": true,
	}
	if len(allow) > 0 {
		permissions["allow"] = allow
	}
	network := map[string]any{"allowedDomains": []string{}, "strictAllowlist": true}
	if o.Sandbox.Loopback {
		// No domain is added: a loopback domain rule would route through the
		// sandbox's proxy, which is the path to every other allowed domain.
		network["allowLocalBinding"] = true
	}
	sandbox := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"allowUnsandboxedCommands": false,
		"autoAllowBashIfSandboxed": true,
		"network":                  network,
	}
	if len(filesystem) > 0 {
		sandbox["filesystem"] = filesystem
	}
	document := map[string]any{"sandbox": sandbox, "disableAllHooks": true, "permissions": permissions, "claudeMdExcludes": claudeInstructionExcludes(o)}
	if o.Sandbox.Tools != nil || o.Browser {
		// With hosted tools or a browser, MCP is no longer denied wholesale, so
		// the connectors a subscription login would fetch are switched off too.
		document["disableClaudeAiConnectors"] = true
	}
	raw, _ := json.Marshal(document)
	return string(raw)
}

// claudeInstructionExcludes names the operator's own instruction files: their
// user-level CLAUDE.md and rules, and any in a directory above the workspace.
// With every settings source dropped, Claude Code 2.1.280 loads no instruction
// files at all, including the workspace's own, so this is a second guard for
// builds where that changes. A caller that wants a repository's instructions
// followed has to point the session at them.
func claudeInstructionExcludes(o Options) []string {
	homes := []string{o.Provider.CLI.Home}
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, filepath.Join(home, ".claude"))
	}
	var out []string
	for _, home := range homes {
		if home != "" {
			out = append(out, filepath.Join(home, "CLAUDE.md"), filepath.Join(home, "rules")+"/**")
		}
	}
	for dir := filepath.Dir(o.WorkDir); ; dir = filepath.Dir(dir) {
		out = append(out, filepath.Join(dir, "CLAUDE.md"), filepath.Join(dir, "CLAUDE.local.md"), filepath.Join(dir, ".claude", "CLAUDE.md"), filepath.Join(dir, ".claude", "rules")+"/**")
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return out
}

func claudeSandboxArgs(o Options) []string {
	var disallowed []string
	if !o.Sandbox.Web {
		disallowed = append(disallowed, claudeWebTools...)
	}
	// A deny outranks every allow in Claude Code, so denying MCP wholesale
	// would deny the hosted tools and the browser's too. With either,
	// --strict-mcp-config loads only their servers and dontAsk refuses any
	// tool without an allow rule.
	if o.Sandbox.Tools == nil && !o.Browser {
		disallowed = append(disallowed, "mcp__*")
	}
	if o.Browser {
		disallowed = append(disallowed, claudeBrowserTools(claudeBrowserWithheld)...)
	}
	args := []string{"--setting-sources=", "--strict-mcp-config", "--disable-slash-commands"}
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowed, ","))
	}
	return append(args, "--settings", claudeSandboxSettings(o))
}
