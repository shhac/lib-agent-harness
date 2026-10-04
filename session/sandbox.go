package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
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
	// Loopback lets the session's shell bind and connect to this machine's own
	// addresses and nothing else, so it can start a dev server and request it.
	// Claude Code's allowLocalBinding admits the machine's interface addresses
	// as well as 127.0.0.0/8 and ::1, so a server bound to one of those is
	// reachable too; no other host is. Everything the project needs at run
	// time must then be local. It is proved before launch — a canary inside
	// the sandbox must reach a loopback listener, bind one of its own, and be
	// refused an off-machine address the probe reached itself — or the session
	// is refused. Claude Code offers it; Codex 0.160.0 is refused: owner
	// macOS experiments found closed networking also refused loopback, while
	// enabled variants and a native session allowed off-machine TCP/UDP
	// port 53. Other platforms have no native enforcement proof (see
	// harness.Support).
	Loopback bool
	// Tools adds the caller's tools beside the session's own, served through
	// the same bridge and tool channel a restricted session uses, with the same
	// lease, launch record and reclamation. Its Dir must lie outside WorkDir.
	// The bridge runs outside the OS sandbox, as the CLI's MCP servers do, so a
	// hosted tool reaches whatever its handler does; the sandbox confines only
	// the session's own tools.
	//
	// Every other MCP server stays off: Claude Code loads only this one, with
	// claude.ai connectors disabled and any other server's tools refused by
	// dontAsk, and its startup report must list this server's tools and no
	// other server's. Codex's private runtime home declares no other server
	// except node_repl when Browser is set, with JavaScript confinement proved
	// before launch.
	Tools *ToolHost
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

func sandboxReadDirs(dirs []string) ([]string, string) { return sandboxbridge.ReadDirs(dirs) }

// normalizeSandbox freezes the caller's sandbox and rejects every setting a
// sandbox would otherwise have to silently override. o.Policy is the caller's,
// before defaults are applied.
func normalizeSandbox(o Options) (Options, error) {
	frozen := *o.Sandbox
	frozen.Read = append([]string(nil), o.Sandbox.Read...)
	if o.Sandbox.Tools != nil {
		// Copied for the same reason a restriction is: normalizing must not edit
		// the caller's value, and a launch must not see another's edits.
		tools := *o.Sandbox.Tools
		tools.Tools = freezeTools(tools.Tools)
		frozen.Tools = &tools
	}
	o.Sandbox = &frozen
	if o.Restriction != nil {
		return o, refuse(o, "sandbox", RefusedConflict, "a session is either restricted or sandboxed; set only one of Restriction and Sandbox")
	}
	// Sandbox rules name the working directory by path. A path reached through
	// a symlink would not match the one the sandbox resolves, so name the real
	// directory.
	if resolved, err := filepath.EvalSymlinks(o.WorkDir); err == nil {
		o.WorkDir = resolved
	}
	if !restrictedPlatform() {
		return o, &CapabilityError{Engine: o.Provider.Engine, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	read, problem := sandboxReadDirs(o.Sandbox.Read)
	if problem != "" {
		return o, refuse(o, "sandbox", RefusedSandboxRead, problem)
	}
	o.Sandbox.Read = read
	if o.Sandbox.Loopback {
		if c := harness.Support(o.Provider.Engine, harness.Session, harness.Loopback); !c.Usable() {
			return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "loopback", Code: RefusedNotOffered, Capability: c}
		}
	}
	if err := validateSandboxTools(o); err != nil {
		return o, err
	}
	if o.Provider.Engine == harness.Codex {
		if o.Policy.CodexSandbox != "" {
			return o, refuse(o, "policy", RefusedConflict, "a sandboxed Codex session owns its sandbox; leave Policy.CodexSandbox unset")
		}
		if o.Policy.CodexApproval != "" && o.Policy.CodexApproval != "never" {
			return o, refuse(o, "policy", RefusedConflict, "a sandboxed Codex session never asks for approval; leave Policy.CodexApproval unset or never")
		}
		o.Policy.CodexApproval = "never"
		if o.RuntimeHome == "" {
			return o, refuse(o, "runtime_home", RefusedRuntimeHome, "a sandboxed Codex session requires a durable private runtime home; set Options.RuntimeHome")
		}
		abs, err := filepath.Abs(o.RuntimeHome)
		if err != nil {
			return o, refuse(o, "runtime_home", RefusedRuntimeHome, "invalid sandboxed session runtime home")
		}
		o.RuntimeHome = abs
		return o, nil
	}
	if o.Policy.ClaudePermission != "" && o.Policy.ClaudePermission != "dontAsk" {
		return o, refuse(o, "policy", RefusedConflict, "a sandboxed Claude session runs in dontAsk permission mode; leave Policy.ClaudePermission unset or dontAsk")
	}
	o.Policy.ClaudePermission = "dontAsk"
	if o.Policy.ClaudeTools == nil {
		o.Policy.ClaudeTools = sandboxClaudeTools(o.Sandbox.Write, o.Sandbox.Web)
		return o, nil
	}
	allowed := sandboxClaudeTools(true, o.Sandbox.Web)
	for _, tool := range o.Policy.ClaudeTools {
		if !slices.Contains(allowed, tool) || (!o.Sandbox.Write && (tool == "Edit" || tool == "Write")) {
			return o, refuse(o, "tools", RefusedSandboxTool, "a sandboxed session cannot enable "+tool)
		}
	}
	return o, nil
}

// validateSandboxTools refuses a tool host the session could not run as asked.
func validateSandboxTools(o Options) error {
	tools := o.Sandbox.Tools
	if tools == nil {
		return nil
	}
	if err := tools.validate(); err != nil {
		return toolHostRefusal(o, err)
	}
	if (o.Provider.Engine == harness.Claude && reservedClaudeServer(tools.Server)) || (o.Provider.Engine == harness.Codex && o.Browser && tools.Server == "node_repl") {
		return &CapabilityError{Engine: o.Provider.Engine, Code: CapabilityServerNameReserved, Phase: BeforeLaunch, Tools: []string{tools.Server}}
	}
	// The channel's credential and lock live in Dir. Inside the workspace a
	// writing session's own tools could replace them.
	if pathContains(containmentPath(o.WorkDir), containmentPath(tools.Dir)) {
		return refuse(o, "tools", RefusedConflict, "a sandboxed session's tool host directory must lie outside its working directory")
	}
	return nil
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
	// browser or the desktop — are switched off. Browser opts into exactly
	// two browser features after its private bridge is assembled. The shell
	// and file tools stay,
	// and so does the code-mode host: on codex 0.154.0 every native tool call
	// runs through it, and disabling it leaves a session unable to do anything.
	for _, feature := range sandboxDisabledFeatures {
		settings = append(settings, "features."+feature+"=false")
	}
	return codexOverrides(settings)
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

// sandboxArgs are the arguments that describe a sandboxed session's sandbox,
// and are what its check proves.
func sandboxArgs(o Options) []string {
	if o.Provider.Engine == harness.Codex {
		args := codexSandboxArgs(*o.Sandbox)
		if o.Browser {
			args = append(args, nativecli.CodexBrowserArgs()...)
		}
		return args
	}
	return claudeSandboxArgs(o)
}

// prepareSandbox assembles a sandboxed launch and proves its sandbox against
// the installed harness before a credentialed process exists. A session that
// hosts tools opens its tool channel only once the sandbox is proved, under
// lease, which the launch owns from here.
func prepareSandbox(ctx context.Context, o Options, lease *os.File) (*launch, error) {
	refuse := func(err error) (*launch, error) { _ = lease.Close(); return nil, err }
	l := &launch{extra: sandboxArgs(o)}
	if o.Provider.Engine == harness.Codex {
		if o.Browser {
			bridge, err := readCodexBrowserBridge(ctx, o)
			if err != nil {
				return refuse(err)
			}
			l.browser = bridge
		}
		home := codexRuntime(o.Provider.CLI.Home, o.RuntimeHome)
		if l.browser != nil {
			config, err := l.browser.config(o.RuntimeHome)
			if err != nil {
				return refuse(err)
			}
			home.Files[codexConfigFile] = config
		}
		if err := home.Prepare(); err != nil {
			return refuse(runtimeFailure(err))
		}
		if err := syncRuntimeSkills(o); err != nil {
			return refuse(err)
		}
	}
	if err := verifySandbox(ctx, o, l); err != nil {
		return refuse(err)
	}
	if o.Sandbox.Tools == nil {
		_ = lease.Close()
		return l, nil
	}
	host, err := newToolHost(*o.Sandbox.Tools, lease)
	if err != nil {
		return nil, err
	}
	l.host = host
	if o.Provider.Engine == harness.Claude {
		l.extra = append(l.extra, claudeMCPConfig(host), claudeHostedAllowed(host))
		return l, nil
	}
	server, err := codexHostedServer(host)
	if err != nil {
		host.close()
		return nil, err
	}
	l.extra = append(l.extra, codexOverrides(server)...)
	return l, nil
}

// VerifySandbox proves, without inference, that the installed harness can run
// a session with this sandbox. Start and Resume run the same check; this lets
// a caller report readiness before any work is asked for.
func VerifySandbox(ctx context.Context, o Options) error {
	if o.Sandbox == nil {
		return refuse(o, "sandbox", RefusedNotConfigured, "options carry no sandbox to verify")
	}
	o, err := normalize(o)
	if err != nil {
		return err
	}
	if err = lockedLogin(o); err != nil {
		return err
	}
	if o.Provider.Engine == harness.Codex {
		if login, err := credentialDigest(filepath.Join(o.Provider.CLI.Home, codexCredentialFile)); err != nil || login == nil {
			// Start would refuse to share a missing login; say so here too rather
			// than report a session ready that cannot open.
			return &CapabilityError{Engine: harness.Codex, Code: CapabilityLoginUnavailable, Phase: BeforeLaunch}
		}
	}
	l := &launch{extra: sandboxArgs(o)}
	if o.Provider.Engine == harness.Codex && o.Browser {
		l.browser, err = readCodexBrowserBridge(ctx, o)
		if err != nil {
			return err
		}
	}
	return verifySandbox(ctx, o, l)
}
