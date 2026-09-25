package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Sandbox opts an ordinary native session — its own tools intact — into the
// installed CLI's OS sandbox. Nothing the session executes reaches the network,
// and it writes inside WorkDir only when Write is set. Claude Code's shell may
// also write its own private session temporary directory; Codex's may not write
// /tmp or $TMPDIR at all.
//
// Reads are not fully contained. A Codex session can read what the operating
// account can read. A Claude session's shell cannot read the home directory
// outside WorkDir and Read, but can read the rest of the system. Either way the
// model provider connection remains an outward channel for anything read.
// Claude Code's file tools are confined by permission rules rather than by the
// OS sandbox, which covers only its shell.
//
// The sandbox is proved before any credentialed launch and, for Codex, read
// back from the running harness before its first prompt. There is no mode that
// starts a sandboxed session whose sandbox could not be established.
type Sandbox struct {
	Write bool
	// Read names directories outside the workspace that the session's shell
	// may also read, such as a shared build-module cache. Claude Code blocks its
	// shell from reading the home directory otherwise; Codex already reads
	// everything the account can. Each must be an absolute directory that holds
	// no credentials. None may contain the home directory, which would reopen
	// all of it.
	Read []string
	// Web lets the session search and fetch the web: Claude Code's WebSearch
	// and WebFetch tools, and Codex's web search. It opens an outward channel
	// for anything the session can read. The shell's network stays closed:
	// Claude's WebFetch runs in the CLI process under a permission rule that
	// names no domain, because a domain rule would also open the shell's
	// network, and Codex searches through its provider.
	Web bool
}

// sandboxProfile is the Codex permission profile a sandboxed session runs
// under. The installed app-server applies a profile only when thread/start
// carries no legacy sandbox mode; passing one silently replaces the profile
// with a policy that also writes temporary directories.
const sandboxProfile = "harness_sandbox"

var claudeWebTools = []string{"WebFetch", "WebSearch"}

func sandboxClaudeTools(write, web bool) []string {
	tools := []string{"Bash", "Read", "Glob", "Grep"}
	if write {
		tools = []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep"}
	}
	if web {
		tools = append(tools, claudeWebTools...)
	}
	return tools
}

// sandboxReadDirs resolves each extra readable directory to the path the
// sandbox will match, and refuses any that would reopen the home directory.
func sandboxReadDirs(dirs []string) ([]string, error) {
	home, _ := os.UserHomeDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return nil, fmt.Errorf("sandbox read path %q must be a clean absolute path", dir)
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if rel, err := filepath.Rel(dir, home); home != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("sandbox read path %q would reopen the home directory", dir)
		}
		out = append(out, dir)
	}
	return out, nil
}

// normalizeSandbox freezes the caller's sandbox and rejects every setting a
// sandbox would otherwise have to silently override. o.Policy is the caller's,
// before defaults are applied.
func normalizeSandbox(o Options) (Options, error) {
	frozen := *o.Sandbox
	frozen.Read = append([]string(nil), o.Sandbox.Read...)
	o.Sandbox = &frozen
	if o.Restriction != nil {
		return o, errors.New("a session is either restricted or sandboxed; set only one of Restriction and Sandbox")
	}
	// Sandbox rules name the working directory by path. A path reached through
	// a symlink would not match the one the sandbox resolves, so name the real
	// directory.
	if resolved, err := filepath.EvalSymlinks(o.WorkDir); err == nil {
		o.WorkDir = resolved
	}
	if !restrictedPlatform() {
		return o, &CapabilityError{Engine: string(o.Engine), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	read, err := sandboxReadDirs(o.Sandbox.Read)
	if err != nil {
		return o, err
	}
	o.Sandbox.Read = read
	if o.Engine == Codex {
		if o.Policy.CodexSandbox != "" {
			return o, errors.New("a sandboxed Codex session owns its sandbox; leave Policy.CodexSandbox unset")
		}
		if o.Policy.CodexApproval != "" && o.Policy.CodexApproval != "never" {
			return o, errors.New("a sandboxed Codex session never asks for approval; leave Policy.CodexApproval unset or never")
		}
		o.Policy.CodexApproval = "never"
		if o.RuntimeHome == "" {
			return o, errors.New("a sandboxed Codex session requires a durable private runtime home; set Options.RuntimeHome")
		}
		abs, err := filepath.Abs(o.RuntimeHome)
		if err != nil {
			return o, errors.New("invalid sandboxed session runtime home")
		}
		o.RuntimeHome = abs
		return o, nil
	}
	if o.Policy.ClaudePermission != "" && o.Policy.ClaudePermission != "dontAsk" {
		return o, errors.New("a sandboxed Claude session runs in dontAsk permission mode; leave Policy.ClaudePermission unset or dontAsk")
	}
	o.Policy.ClaudePermission = "dontAsk"
	if o.Policy.ClaudeTools == nil {
		o.Policy.ClaudeTools = sandboxClaudeTools(o.Sandbox.Write, o.Sandbox.Web)
		return o, nil
	}
	allowed := sandboxClaudeTools(true, o.Sandbox.Web)
	for _, tool := range o.Policy.ClaudeTools {
		if !slices.Contains(allowed, tool) || (!o.Sandbox.Write && (tool == "Edit" || tool == "Write")) {
			return o, &UnsupportedError{"tools", Capability{Unsupported, "a sandboxed session cannot enable " + tool}}
		}
	}
	return o, nil
}

var sandboxDisabledFeatures = []string{"apps", "plugins", "remote_plugin", "hooks", "browser_use", "browser_use_external", "computer_use", "multi_agent", "multi_agent_v2", "skill_mcp_dependency_install", "workspace_dependencies", "tool_suggest", "memories"}

// codexSandboxArgs are the overrides a sandboxed Codex harness and its canary
// both run with. They are the only place the profile is described.
func codexSandboxArgs(s Sandbox) []string {
	workspace := "read"
	if s.Write {
		workspace = "write"
	}
	// Codex searches through its provider, not from the shell, so a live
	// search leaves the profile's network closed.
	webSearch := `web_search="disabled"`
	if s.Web {
		webSearch = `web_search="live"`
	}
	settings := []string{
		`default_permissions="` + sandboxProfile + `"`,
		// Configuration a later launch would read is never writable: .git for
		// hooks, .codex and .agents for servers and skills that run outside it.
		`permissions.` + sandboxProfile + `.filesystem={":root"="read",":workspace_roots"={"."="` + workspace + `",".git"="read",".codex"="read",".agents"="read"}}`,
		`permissions.` + sandboxProfile + `.network.enabled=false`,
		`approval_policy="never"`,
		webSearch,
		`agents.enabled=false`,
		`check_for_update_on_startup=false`,
		`analytics.enabled=false`,
	}
	// Surfaces that reach past the sandbox — connectors, plugins, hooks, a
	// browser or the desktop — are switched off. The shell and file tools stay,
	// and so does the code-mode host: on codex 0.154.0 every native tool call
	// runs through it, and disabling it leaves a session unable to do anything.
	for _, feature := range sandboxDisabledFeatures {
		settings = append(settings, "features."+feature+"=false")
	}
	args := make([]string, 0, len(settings)*2)
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	return args
}

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
	if len(o.Sandbox.Read) > 0 {
		filesystem["allowRead"] = o.Sandbox.Read
		for _, dir := range o.Sandbox.Read {
			allow = append(allow, "Read(/"+dir+"/**)")
		}
	}
	if o.Sandbox.Web {
		// Bare tool names, never WebFetch(domain:...): Claude Code adds every
		// domain such a rule names to the shell's network allowlist too.
		// dontAsk refuses a tool with no allow rule, so these are required.
		allow = append(allow, claudeWebTools...)
	}
	permissions := map[string]any{
		"defaultMode":                         "dontAsk",
		"deny":                                deny,
		"blockReadsOutsideWorkingDirectories": true,
	}
	if len(allow) > 0 {
		permissions["allow"] = allow
	}
	sandbox := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"allowUnsandboxedCommands": false,
		"autoAllowBashIfSandboxed": true,
		"network":                  map[string]any{"allowedDomains": []string{}, "strictAllowlist": true},
	}
	if len(filesystem) > 0 {
		sandbox["filesystem"] = filesystem
	}
	raw, _ := json.Marshal(map[string]any{"sandbox": sandbox, "disableAllHooks": true, "permissions": permissions, "claudeMdExcludes": claudeInstructionExcludes(o)})
	return string(raw)
}

// claudeInstructionExcludes names the operator's own instruction files: their
// user-level CLAUDE.md and rules, and any in a directory above the workspace.
// With every settings source dropped, Claude Code 2.1.280 loads no instruction
// files at all, including the workspace's own, so this is a second guard for
// builds where that changes. A caller that wants a repository's instructions
// followed has to point the session at them.
func claudeInstructionExcludes(o Options) []string {
	homes := []string{o.Home}
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
	disallowed = append(disallowed, "mcp__*")
	return []string{"--setting-sources=", "--strict-mcp-config", "--disable-slash-commands", "--disallowedTools", strings.Join(disallowed, ","), "--settings", claudeSandboxSettings(o)}
}

// prepareSandbox assembles a sandboxed launch and proves its sandbox against
// the installed harness before a credentialed process exists.
func prepareSandbox(ctx context.Context, o Options) (*launch, error) {
	l := &launch{}
	if o.Engine == Codex {
		if _, err := prepareRuntimeHome(o.Home, o.RuntimeHome); err != nil {
			return nil, err
		}
		l.extra = codexSandboxArgs(*o.Sandbox)
	} else {
		l.extra = claudeSandboxArgs(o)
	}
	if err := verifySandbox(ctx, o, l); err != nil {
		return nil, err
	}
	return l, nil
}

// VerifySandbox proves, without inference, that the installed harness can run
// a session with this sandbox. Start and Resume run the same check; this lets
// a caller report readiness before any work is asked for.
func VerifySandbox(ctx context.Context, o Options) error {
	if o.Sandbox == nil {
		return errors.New("options carry no sandbox to verify")
	}
	o, err := normalize(o)
	if err != nil {
		return err
	}
	l := &launch{extra: codexSandboxArgs(*o.Sandbox)}
	if o.Engine == Claude {
		l.extra = claudeSandboxArgs(o)
	} else if login, err := credentialDigest(filepath.Join(o.Home, codexCredentialFile)); err != nil || login == nil {
		// Start would refuse to share a missing login; say so here too rather
		// than report a session ready that cannot open.
		return &CapabilityError{Engine: string(Codex), Code: CapabilityLoginUnavailable, Phase: BeforeLaunch}
	}
	return verifySandbox(ctx, o, l)
}
