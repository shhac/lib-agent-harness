package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"

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
	// Loopback requests proved on-machine networking. The base Claude proof
	// must pass before launch. On macOS allowLocalBinding permits binds and
	// inbound connections on every local interface, not local-only binds.
	// Optional interface diagnostics do not gate that base capability.
	Loopback bool
	// LoopbackLocalOnly requires Loopback and refuses unproved local-only binds.
	LoopbackLocalOnly bool
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
	if o.Sandbox.LoopbackLocalOnly {
		if !o.Sandbox.Loopback {
			return o, refuse(o, "loopback", RefusedConflict, "LoopbackLocalOnly requires Loopback")
		}
		c := harness.Support(o.Provider.Engine, harness.Session, harness.LoopbackLocalOnly)
		if !c.Usable() {
			return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "loopback", Code: RefusedLoopbackNotLocal, Capability: c}
		}
	}
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

// sandboxArgs are the arguments that describe a sandboxed session's sandbox,
// and are what its check proves.

// prepareSandbox assembles a sandboxed launch and proves its sandbox against
// the installed harness before a credentialed process exists. A session that
// hosts tools opens its tool channel only once the sandbox is proved, under
// lease, which the launch owns from here.
func prepareSandbox(ctx context.Context, o Options, lease *os.File) (*launch, error) {
	refuse := func(err error) (*launch, error) { _ = lease.Close(); return nil, err }
	support := engines[o.Provider.Engine].sandbox
	if support == nil {
		return refuse(&UnsupportedError{Engine: o.Provider.Engine, Operation: "sandbox", Code: RefusedNotOffered, Capability: harness.Support(o.Provider.Engine, harness.Session, harness.Sandbox)})
	}
	l := &launch{extra: support.args(o)}
	if support.prepare != nil {
		if err := support.prepare(ctx, o, l); err != nil {
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
	hosted, err := support.hostTools(host)
	if err != nil {
		host.close()
		return nil, err
	}
	l.extra = append(l.extra, hosted...)
	return l, nil
}

// sandboxArgs are the arguments that put the engine's CLI in its sandbox.
func sandboxArgs(o Options) []string { return engines[o.Provider.Engine].sandbox.args(o) }

// sandboxSupport is how one engine runs sandboxed. prepareSandbox owns the
// order: arguments, preparation, proof, and only after the proof the tool
// channel.
type sandboxSupport struct {
	args func(Options) []string
	// prepare readies what the sandboxed CLI reads before it is proved, such
	// as Codex's runtime home and browser bridge.
	prepare func(context.Context, Options, *launch) error
	// probe proves the sandbox the arguments describe, with a disposable
	// login and no inference, returning optional private network observations.
	probe func(context.Context, Options, *launch) (loopbackEvidence, error)
	// hostTools are the arguments that give the session the open tool
	// channel.
	hostTools func(*toolHost) ([]string, error)
}

var (
	codexSandbox = &sandboxSupport{
		args: func(o Options) []string {
			args := codexSandboxArgs(*o.Sandbox)
			if o.Browser {
				args = append(args, nativecli.CodexBrowserArgs()...)
			}
			return args
		},
		prepare: prepareCodexSandbox,
		probe: func(ctx context.Context, o Options, l *launch) (loopbackEvidence, error) {
			if err := probeCodexSandbox(ctx, o); err != nil || !o.Browser {
				return loopbackEvidence{}, err
			}
			return loopbackEvidence{}, probeCodexBrowserSandbox(ctx, o, l)
		},
		hostTools: func(host *toolHost) ([]string, error) {
			server, err := codexHostedServer(host)
			if err != nil {
				return nil, err
			}
			return codexOverrides(server), nil
		},
	}
	claudeSandbox = &sandboxSupport{
		args: claudeSandboxArgs,
		probe: func(ctx context.Context, o Options, l *launch) (loopbackEvidence, error) {
			if err := probeClaudeSandbox(ctx, o); err != nil || !o.Sandbox.Loopback {
				return loopbackEvidence{}, err
			}
			return probeClaudeLoopback(ctx, o, l)
		},
		hostTools: func(host *toolHost) ([]string, error) {
			return []string{claudeMCPConfig(host), claudeHostedAllowed(host)}, nil
		},
	}
)

// prepareCodexSandbox readies the runtime home a sandboxed Codex session runs
// in, with the browser bridge's configuration when the session has a browser.
func prepareCodexSandbox(ctx context.Context, o Options, l *launch) error {
	if o.Browser {
		bridge, err := readCodexBrowserBridge(ctx, o)
		if err != nil {
			return err
		}
		l.browser = bridge
	}
	home := codexRuntime(o.Provider.CLI.Home, o.RuntimeHome)
	if l.browser != nil {
		config, err := l.browser.config(o.RuntimeHome)
		if err != nil {
			return err
		}
		home.Files[codexConfigFile] = config
	}
	if err := home.Prepare(); err != nil {
		return runtimeFailure(err)
	}
	return syncRuntimeSkills(o)
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
