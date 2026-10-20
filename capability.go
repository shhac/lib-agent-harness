package harness

import "runtime"

// Availability distinguishes how, and whether, an engine offers something.
type Availability string

const (
	// Native: the harness's own protocol provides it.
	Native Availability = "native"
	// Composed: the library builds it from other native operations.
	Composed Availability = "composed"
	// Unsupported: refused before any work starts.
	Unsupported Availability = "unsupported"
	// Unknown: not established until checked against the installed harness.
	Unknown Availability = "unknown"
)

// Capability is an availability with a library-authored reason.
type Capability struct {
	Availability Availability `json:"availability"`
	Reason       string       `json:"reason,omitempty"`
}

// Usable reports whether a caller may attempt it: native or composed, or
// unknown until the installed harness answers.
func (c Capability) Usable() bool { return c.Availability != Unsupported && c.Availability != "" }

// Operation is a kind of invocation.
type Operation string

const (
	// Complete: the model returns text and proposes the caller's tools; native
	// tools are proven absent (package completion).
	Complete Operation = "complete"
	// Run: one native agent invocation (package native).
	Run Operation = "run"
	// Session: a persistent native agent with turns (package session).
	Session Operation = "session"
	// Models: listing the models and efforts an engine offers.
	Models Operation = "models"
	// Account: login, plan, quota windows and credits.
	Account Operation = "account"
)

// Operations lists every operation, in a stable order for display.
func Operations() []Operation { return []Operation{Complete, Run, Session, Models, Account} }

// Feature is something an operation may offer.
type Feature string

const (
	// Available is the operation itself.
	Available      Feature = "available"
	WorkspaceRead  Feature = "workspace_read"  // confined read-only workspace tools
	WorkspaceWrite Feature = "workspace_write" // workspace mutation

	Effort           Feature = "effort"
	Tools            Feature = "tools"             // caller-defined tools: proposed (Complete) or hosted (Session)
	StructuredOutput Feature = "structured_output" // a report constrained by the caller's JSON schema
	// ProgressMessages: before its final report the agent may send messages
	// the schema does not constrain. Codex applies the schema to every message.
	ProgressMessages    Feature = "progress_messages"
	Resume              Feature = "resume"
	Interrupt           Feature = "interrupt"
	Steer               Feature = "steer"
	Compact             Feature = "compact"
	AppendInstructions  Feature = "append_instructions"
	ReplaceInstructions Feature = "replace_instructions"
	RestrictTools       Feature = "restrict_tools" // only configured tools, proven before launch
	Sandbox             Feature = "sandbox"        // native tools or workbench commands inside a proven OS sandbox
	CostReport          Feature = "cost"           // the harness values its token spend
	CacheSplit          Feature = "cache_split"    // cached input is reported apart from fresh
	ContextWindow       Feature = "context_window" // the serving model's window is stated
	// MaxOutputTokens: a caller-set cap on the tokens one response may produce.
	MaxOutputTokens Feature = "max_output_tokens"
	Login           Feature = "login"
	Quota           Feature = "quota"   // subscription usage windows
	Credits         Feature = "credits" // prepaid or overage balance
	// ProvidedSkills: caller-provided skills (Skills.Provided) made available
	// for one invocation. Native where the harness loads them itself, composed
	// where the library does. (The names Skills and GlobalSkills are the
	// request types in skills.go.)
	ProvidedSkills Feature = "skills"
	// IncludeGlobalSkills: including the harness's own installed skills
	// (Skills.Global Include).
	IncludeGlobalSkills Feature = "global_skills"
	// ToolActivity: session tool events carry the tool's arguments and its
	// result text, bounded, as the harness reported them.
	ToolActivity Feature = "tool_activity"
	// Loopback permits on-machine outbound networking, proved before launch.
	// On macOS binds and inbound are allowed on every local interface: a server
	// bound to 0.0.0.0 or a LAN address may be reachable from other machines.
	// Claude Linux sessions offer per-command loopback: host and other-command
	// servers are outside the contract; host-interface bind confinement is unproved.
	Loopback Feature = "loopback"
	// LoopbackLocalOnly requires binds, inbound and outbound confined to loopback.
	// Standalone Linux sandbox.Open proves private namespace loopback; native
	// and workbench sessions do not offer this stricter request.
	LoopbackLocalOnly Feature = "loopback_local_only"
	// LoopbackPorts requests selected-port loopback confinement; unsupported everywhere.
	LoopbackPorts Feature = "loopback_ports"
	// Browser: the browser integration the harness itself ships, switched on
	// for this invocation. Off unless asked for.
	Browser Feature = "browser"
	// SandboxedBrowser: Browser is admitted in a session that also sets
	// Sandbox. A caller that runs every session sandboxed asks this before
	// offering the browser at all.
	SandboxedBrowser Feature = "sandboxed_browser"
	// Background: the harness and everything it starts run at background
	// priority, so agent work yields to the machine's interactive use.
	Background Feature = "background"
	// ToolImages: tool_completed events carry the images a tool returned,
	// such as a browser screenshot, decoded and bounded.
	ToolImages Feature = "tool_images"
)

type supportKey struct {
	engine    Engine
	operation Operation
	feature   Feature
}

// Support is the library's static claim about an engine. Unknown entries are
// promoted only by evidence from the installed harness; nothing promotes past
// this table. A caller asks it instead of comparing engine names.
func Support(e Engine, op Operation, f Feature) Capability {
	return supportFor(runtime.GOOS, e, op, f)
}

// supportFor is Support's claim for the platform goos names.
func supportFor(goos string, e Engine, op Operation, f Feature) Capability {
	if f == ProcessInspection {
		return processInspectionSupport(e, op, goos)
	}
	if f == LoopbackPorts {
		return loopbackPortsSupport(e, op, goos)
	}
	if e.Transport() == "" {
		return Capability{Unsupported, "unrecognized engine"}
	}
	if f == LoopbackLocalOnly {
		return loopbackLocalOnlySupport(goos, e, op)
	}
	if f != Available {
		if operation := supportFor(goos, e, op, Available); !operation.Usable() {
			return operation
		}
	}
	if c, ok := supportTable[supportKey{e, op, f}]; ok {
		return platform(goos, e, op, f, c)
	}
	return Capability{Unsupported, "not offered by this engine"}
}

// platform applies what the operating system rules out: restricted and
// sandboxed hosting of a CLI harness are unavailable on Windows. An API
// session has no process to contain, so it is unaffected by that, though its
// workbench depends on the platform's command sandbox.
func platform(goos string, e Engine, op Operation, f Feature, c Capability) Capability {
	apiSession := e == OpenAICompatible && op == Session
	switch goos {
	case "linux":
		return linuxPlatform(e, op, f, c, apiSession)
	case "darwin":
		return darwinPlatform(e, op, f, c, apiSession)
	case "windows":
		return windowsPlatform(e, op, f, c, apiSession)
	}
	if apiSession && (f == Sandbox || f == Loopback || f == Background) {
		return apiCommandsUnavailable
	}
	if f == WorkspaceRead || f == WorkspaceWrite {
		return Capability{Unsupported, "workspace mount checks are unavailable on this platform"}
	}
	return c
}

var (
	apiCommandsUnavailable = Capability{Unsupported, "API commands require a proved macOS Seatbelt sandbox; this platform has no command sandbox"}
	workbenchFilesOff      = Capability{Unsupported, "workbench file tools are off until their workspace check is verified"}
)

func linuxPlatform(e Engine, op Operation, f Feature, c Capability, apiSession bool) Capability {
	if e == Claude && op == Session && f == Loopback {
		return Capability{Unknown, claudeLinuxLoopbackReason}
	}
	if !apiSession {
		return c
	}
	switch f {
	case Sandbox:
		return Capability{Unknown, "Workbench.Commands proves bubblewrap 0.8.0+ without capabilities or nested user namespaces before launch or refuses the session"}
	case Loopback:
		return Capability{Unsupported, "each command runs in its own bubblewrap network namespace, so a server one command starts is gone before the next; a single command may still start and request its own server"}
	case Background:
		return Capability{Composed, "Workbench.Commands runs each contained process group at nice 10"}
	case WorkspaceRead, WorkspaceWrite:
		return workbenchFilesOff
	}
	return c
}

func darwinPlatform(e Engine, op Operation, f Feature, c Capability, apiSession bool) Capability {
	if e == Claude && op == Session && f == Loopback {
		return Capability{Unknown, claudeLoopbackExposureReason}
	}
	if apiSession && (f == WorkspaceRead || f == WorkspaceWrite) {
		return workbenchFilesOff
	}
	return c
}

func windowsPlatform(e Engine, op Operation, f Feature, c Capability, apiSession bool) Capability {
	if apiSession {
		switch f {
		case Sandbox, Loopback, Background:
			return apiCommandsUnavailable
		case WorkspaceWrite:
			c.Reason += "; not directory-synced; new files inherit the directory ACL"
		}
		return c
	}
	if op == Session && e.Transport() == CLITransport && (f == RestrictTools || f == Sandbox || f == Tools || f == Loopback) {
		if e == Codex && f == Loopback {
			return Capability{Unsupported, "restricted and sandboxed hosting are unavailable on Windows; " + c.Reason}
		}
		return Capability{Unsupported, "restricted and sandboxed hosting are unavailable on Windows"}
	}
	if f == Background {
		return Capability{Unsupported, "background priority is not implemented on Windows"}
	}
	if e == CommandCode && op == Session {
		return Capability{Unsupported, "Command Code sessions are not verified on Windows, where cmd names the system command interpreter"}
	}
	if e == Grok && op == Complete {
		return Capability{Unsupported, "Grok's restricted runtime home is unavailable on Windows"}
	}
	return c
}
