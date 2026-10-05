package session

// The session's failure vocabulary: the sentinels a caller matches with
// errors.Is and the typed failures it inspects with errors.As. Codes are fixed
// library constants; no provider text, path or credential ever enters one.
// Failures that belong to one subsystem's protocol stay with it: the tool
// channel's in tools.go, and crash recovery's in reclaim.go.

import (
	"errors"
	"strconv"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
)

var (
	ErrUnsupported = errors.New("harness operation unsupported")
	ErrClosed      = errors.New("harness session closed")
	ErrBusy        = errors.New("harness turn already active")
	ErrStaleTurn   = errors.New("harness turn does not match expected active turn")
	// ErrToolsUnsettled reports an attempt to start work while a tool call from
	// the previous turn is still outstanding. Cancelling a call asks its handler
	// to stop; until the handler returns, what it did is unknown, and authorizing
	// another turn on top of it would be building on a workspace still in motion.
	ErrToolsUnsettled     = errors.New("harness tool calls from the previous turn have not settled")
	ErrIncompatibleResume = errors.New("harness resume configuration does not match reference")
	ErrBackpressure       = errors.New("harness event buffer exhausted; consume Events while the turn runs")
	ErrProtocol           = errors.New("invalid harness protocol response")
	// ErrRejected is a definitive server refusal. No retry is automatic, but
	// the session remains usable. Provider error text is intentionally omitted.
	ErrRejected    = errors.New("harness rejected operation")
	ErrOutputLimit = errors.New("harness output exceeded its bounded frame or text limit")
	ErrTransport   = errors.New("harness transport failed")
	ErrTurnFailed  = errors.New("harness turn failed")
	// ErrLeaseHeld reports that another process already holds this session's
	// assignment lease. Two processes driving one assignment would spend the
	// same account twice; the second one stops instead.
	ErrLeaseHeld = errors.New("another process holds this session's assignment lease")
)

// UnsupportedError refuses something a session cannot do as configured, before
// it is attempted: a setting the engine cannot honour, a value no engine
// accepts, or a control the installed harness lacks. Operation names the
// option or control; Code is a fixed Refused* constant, and Capability says
// how, and whether, the thing asked for is available.
type UnsupportedError struct {
	Engine     harness.Engine
	Operation  string
	Code       string
	Capability harness.Capability
}

func (e *UnsupportedError) Error() string { return e.Operation + ": " + e.Capability.Reason }
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// errConversationGone marks a resume that failed because the harness no longer
// has the conversation, as opposed to one that failed for any other reason.
var errConversationGone = errors.New("harness conversation is unavailable to resume")

// Refusal codes an UnsupportedError carries. Like every other code they are
// fixed library constants. A refusal about what the engine or library can
// honour is a capability failure; one about a value or this machine's setup is
// a preflight failure (see refusalFamily).
const (
	// RefusedEngine: harness.Support offers no session for the engine.
	RefusedEngine = "unsupported_engine"
	// RefusedOtherEnginePolicy: a Policy field that only another engine reads.
	RefusedOtherEnginePolicy      = "policy_for_other_engine"
	RefusedPolicy                 = "policy_invalid"
	RefusedInstructionMode        = "instruction_mode_invalid"
	RefusedInstructionModeMissing = "instruction_mode_required"
	RefusedWorkDir                = sandbox.RefusedWorkDir
	RefusedHome                   = "home_unavailable"
	RefusedRuntimeHome            = sandbox.RefusedRuntimeHome
	RefusedLimit                  = sandbox.RefusedLimit
	RefusedEnvMalformed           = "env_malformed"
	// RefusedEnvManaged: an environment addition the harness manages itself,
	// or one that would change the CLI outside its sandbox.
	RefusedEnvManaged = "env_managed"
	RefusedToolHost   = "tool_host_invalid"
	// RefusedConflict: settings that would silently disagree, such as a
	// restriction beside a sandbox, or a policy a sandbox or restriction owns.
	RefusedConflict      = sandbox.RefusedConflict
	RefusedModelRequired = "model_required"
	// RefusedModelWithoutTools: Options.CatalogModel lists the model's request
	// parameters and "tools" is not among them, so it cannot take the tools
	// every API session sends.
	RefusedModelWithoutTools   = "model_without_tool_calling"
	RefusedSandboxRead         = sandbox.RefusedSandboxRead
	RefusedSandboxTool         = "sandbox_tool_unsupported"
	RefusedNotConfigured       = "not_configured"
	RefusedNotNative           = "not_native"
	RefusedMethodMissing       = "method_unavailable"
	RefusedNotOffered          = sandbox.RefusedNotOffered
	RefusedWorkbenchMountCheck = sandbox.RefusedWorkbenchMountCheck
	// RefusedKeychainUnavailable: the engine's login lives in a keychain that
	// is locked, so launching it would raise an unlock prompt.
	RefusedKeychainUnavailable = harness.CodeKeychainUnavailable
)

// refusalFamily says whether a refusal is about what can be honoured or about
// a value or this machine. A provider's own problem codes are preflight.
func refusalFamily(code string) harness.Family {
	switch code {
	case RefusedEngine, RefusedOtherEnginePolicy, RefusedEnvManaged, RefusedConflict, RefusedModelRequired,
		RefusedModelWithoutTools, RefusedSandboxTool, RefusedNotNative, RefusedMethodMissing, RefusedNotOffered, RefusedLoopbackNotLocal:
		return harness.FailureCapability
	}
	return harness.FailurePreflight
}

// refuse builds an option refusal: nothing is available as configured.
func refuse(o Options, operation, code, reason string) *UnsupportedError {
	return &UnsupportedError{Engine: o.Provider.Engine, Operation: operation, Code: code, Capability: harness.Capability{Availability: harness.Unsupported, Reason: reason}}
}

// lockedLogin refuses a launch that would read a locked keychain, or is nil.
func lockedLogin(o Options) error {
	if !harness.LoginStoreLocked(o.Provider.Engine) {
		return nil
	}
	return refuse(o, "login", RefusedKeychainUnavailable, "the engine's login is in a locked keychain; launching it would ask the user to unlock it")
}

// toolHostRefusal refuses a tool host that failed validation. The reason is
// one of validate's own fixed messages.
func toolHostRefusal(o Options, err error) *UnsupportedError {
	return refuse(o, "tools", RefusedToolHost, err.Error())
}

// Capability failure codes. They are fixed library constants: no provider text,
// path or credential ever enters one.
const (
	CapabilityUnsupportedPlatform = "restricted_session_unsupported_platform"
	CapabilityNativeToolsPresent  = "native_tools_present"
	// CapabilitySandboxToolMissing: the OS sandbox executable is unavailable.
	CapabilitySandboxToolMissing           = sandbox.CapabilitySandboxToolMissing
	CapabilitySandboxToolOutdated          = sandbox.CapabilitySandboxToolOutdated
	CapabilitySandboxNamespacesUnavailable = sandbox.CapabilitySandboxNamespacesUnavailable
	CapabilityHostedToolsMissing           = "hosted_tools_missing"
	CapabilityInstructionsMerged           = "inherited_instructions_merged"
	CapabilityChangedModel                 = "changed_model"
	CapabilityChangedEffort                = "changed_effort"
	CapabilityProbeNoRequest               = "probe_made_no_request"
	CapabilityProbeUnreadable              = "probe_request_unreadable"
	CapabilityProbeTimeout                 = sandbox.CapabilityProbeTimeout
	CapabilityProbeFailed                  = "probe_could_not_run"
	CapabilityCatalogUnavailable           = "model_catalog_unavailable"
	CapabilityCatalogRestriction           = "model_catalog_restriction_failed"
	CapabilityServerNotLoaded              = "tool_server_not_loaded"
	CapabilityServerNameReserved           = "tool_server_name_reserved"
	CapabilityLoginUnavailable             = "harness_login_unavailable"
	// CapabilityBrowserToolsMissing: startup did not advertise the configured
	// browser integration's tools. This does not identify extension connectivity.
	CapabilityBrowserToolsMissing       = "browser_tools_missing"
	CapabilityBrowserBridgeUnavailable  = "browser_bridge_unavailable"
	CapabilityBrowserSandboxUnproven    = "browser_sandbox_unproven"
	CapabilityBrowserSandboxNotEnforced = "browser_sandbox_not_enforced"
	// CapabilityChangedPermissionMode: the session could not be put in the
	// permission mode its policy relies on.
	CapabilityChangedPermissionMode = "changed_permission_mode"
)

// Sandbox capability failures.
const (
	// CapabilityLoopbackClaimChanged: Seatbelt binds no longer match the claim.
	CapabilityLoopbackClaimChanged = sandbox.CapabilityLoopbackClaimChanged
	// CapabilitySandboxUnavailable: the installed harness could not be put
	// under the requested sandbox, or the check could not be completed.
	CapabilitySandboxUnavailable = sandbox.CapabilitySandboxUnavailable
	// CapabilitySandboxNotEnforced: the sandbox the harness reported, or a
	// canary run under it, allowed something the session must not do.
	CapabilitySandboxNotEnforced = sandbox.CapabilitySandboxNotEnforced
)

// Capability check phases. The distinction matters to an operator: one of these
// happened before the harness existed, the other after it started but still
// before it was given anything to do.
const (
	// BeforeLaunch: the check ran against a disposable home and a provider that
	// refuses inference. No credentialed process was started.
	BeforeLaunch = "before_launch"
	// BeforeFirstPrompt: the harness had started and advertised a surface that
	// disagreed with the session's. No prompt was sent; the session was closed.
	BeforeFirstPrompt = "before_first_prompt"
	// DuringSession: the harness reported a change, after the session was
	// established, that the session's configuration does not allow. The
	// session was stopped; a turn that was running may already have done work.
	DuringSession = "during_session"
)

// CapabilityError reports that a requested session capability could not be established.
// Tool names, when present, are the ones the check disagreed about, and they
// come from the caller's own configuration or from a fixed native-name
// comparison — never from free text.
type CapabilityError struct {
	// ProofStep is a fixed sandbox.ProofStep value, or empty when unknown.
	ProofStep string
	Engine    harness.Engine
	Code      string
	Phase     string
	Tools     []string
	// Reason is a fixed structural refusal, never raw CLI output. Browser
	// failures use Bridge* values; loopback failures name the failed contract.
	Reason string
}

// Reasons for an unavailable bridge contain no paths or CLI output.
const (
	BridgeHomeUnavailable        = "bridge_home_unavailable"
	BridgeNotDeclared            = "bridge_not_declared"
	BridgeDeclarationUnsupported = "bridge_declaration_unsupported"
	BridgeInstallationMissing    = "bridge_installation_missing"
	BridgeInWorkspace            = "bridge_in_workspace"
)

func (e *CapabilityError) Error() string {
	message := map[string]string{
		CapabilitySandboxToolMissing:        "the required OS sandbox tool is missing; install a supported runtime",
		CapabilityUnsupportedPlatform:       "restricted worker sessions are not available on this platform",
		CapabilityNativeToolsPresent:        "the installed harness kept tools this session did not configure",
		CapabilityHostedToolsMissing:        "the installed harness did not offer the tools this session configured",
		CapabilityInstructionsMerged:        "the installed harness merged inherited instructions into its request",
		CapabilityChangedModel:              "the installed harness requested a different model",
		CapabilityChangedEffort:             "the installed harness requested a different reasoning effort",
		CapabilityChangedPermissionMode:     "the installed harness did not accept the permission mode this session requires",
		CapabilityProbeNoRequest:            "the installed harness made no request during the capability check",
		CapabilityProbeUnreadable:           "the capability check could not read the harness's request",
		CapabilityProbeTimeout:              "the capability check did not finish in time",
		CapabilityProbeFailed:               "the capability check could not be run",
		CapabilityCatalogUnavailable:        "the installed harness did not supply a model catalog to restrict",
		CapabilityCatalogRestriction:        "the selected model could not be restricted in the installed harness catalog",
		CapabilityServerNotLoaded:           "the installed harness did not load this session's tool server",
		CapabilityServerNameReserved:        "the installed harness reserves this tool server name; choose another",
		CapabilityLoginUnavailable:          "the selected harness home has no file-backed login to share with a restricted session; log in to that home first",
		CapabilityBrowserBridgeUnavailable:  "the browser bridge home (BrowserBridgeHome, or the CLI home) has no usable ChatGPT node_repl bridge; configure the ChatGPT app browser bridge and Chrome extension, then restart the CLI",
		CapabilityBrowserSandboxUnproven:    "the node_repl JavaScript confinement proof could not complete; update Codex and the ChatGPT app, or leave Browser unset",
		CapabilityBrowserSandboxNotEnforced: "node_repl JavaScript escaped the session sandbox; update Codex and the ChatGPT app, or leave Browser unset",
		CapabilitySandboxUnavailable:        "the installed harness could not be run under the requested sandbox",
		CapabilityLoopbackClaimChanged:      "Seatbelt interface bind observations no longer match the all-interface Loopback claim; re-audit the installed runtime",
		CapabilitySandboxNotEnforced:        "the installed harness's sandbox allowed writes or network access the session must not have",
	}[e.Code]
	if e.Code == CapabilitySandboxNotEnforced && capabilityProofStep(e.ProofStep) != "" {
		message = "the installed harness's sandbox allowed reads, execution, writes or network access the session must not have"
	}
	if len(e.Tools) == 1 && e.Tools[0] == "bwrap" {
		switch e.Code {
		case CapabilitySandboxToolMissing:
			message = "bubblewrap is required; install it with apt install bubblewrap, dnf install bubblewrap or pacman -S bubblewrap"
		case CapabilitySandboxToolOutdated:
			message = "bubblewrap 0.8.0 or later is required for --disable-userns; install the distribution's package or a newer build"
		case CapabilitySandboxNamespacesUnavailable:
			message = "bubblewrap cannot create unprivileged user namespaces; check kernel.unprivileged_userns_clone, user.max_user_namespaces and Ubuntu 24.04+'s kernel.apparmor_restrict_unprivileged_userns and AppArmor profile for bwrap"
		}
	}
	if message == "" {
		message = "the restricted session configuration could not be established"
	}
	if e.Code == CapabilitySandboxUnavailable && e.Reason == claudeLinuxInCommandLoopbackReason {
		message = claudeLinuxInCommandLoopbackReason
	}
	if e.Code == CapabilitySandboxUnavailable && e.Reason == ClaudeLinuxSandboxRequiresSocat {
		message = "claude_linux_sandbox_requires_socat: Claude Code's Linux sandbox requires socat; install it with apt install socat, dnf install socat or pacman -S socat"
	}
	if e.Code == CapabilitySandboxNotEnforced && e.Reason == claudeLinuxWiderScopeReason {
		message = ClaudeLinuxLoopbackScopeWiderThanClaimed + ": Claude Linux must not reach the host's localhost under the per-command contract"
	}
	if e.Code == CapabilityBrowserToolsMissing {
		message = "the installed harness did not advertise the browser tools"
		switch e.Engine {
		case harness.Codex:
			message += "; configure the native Node REPL browser bridge in the selected CLI home, install the ChatGPT Chrome extension (https://chromewebstore.google.com/detail/chatgpt/hehggadaopoacecdllhhajmbjkdcmajg), and restart the CLI"
		case harness.Claude:
			message += "; enable Claude in Chrome, install its Chrome extension (https://chromewebstore.google.com/detail/claude/fcoeoabgfenejglbffodgkkbkcdhcgfn), and restart the CLI"
		}
	}
	if e.Code == CapabilityBrowserBridgeUnavailable {
		if phrase := map[string]string{
			BridgeHomeUnavailable:        "home unavailable",
			BridgeNotDeclared:            "bridge not declared",
			BridgeDeclarationUnsupported: "unsupported declaration",
			BridgeInstallationMissing:    "installation missing",
			BridgeInWorkspace:            "installation overlaps workspace",
		}[e.Reason]; phrase != "" {
			message += " (" + phrase + ")"
		}
	}
	out := string(e.Engine) + ": " + message
	if len(e.Tools) > 0 {
		out += " (" + strings.Join(e.Tools, ", ") + ")"
	}
	if step := capabilityProofStep(e.ProofStep); step != "" {
		out += "; proof step: " + step
	}
	// Say what actually happened rather than one reassuring phrase for both: a
	// session that started and was closed is a different fact to report than one
	// that was never launched.
	switch e.Phase {
	case BeforeFirstPrompt:
		return out + "; the session was closed before any prompt was sent"
	default:
		return out + "; no session was started"
	}
}
func (e *CapabilityError) Unwrap() error { return ErrUnsupported }

// Process failure codes. Like every other code in this library they are fixed
// constants: no harness output is ever promoted into one.
const (
	ProcessExited      = "process_exited"
	ProcessSignalled   = "process_signalled"
	ProcessStartFailed = "process_start_failed"
)

// ProcessError reports that the harness process itself ended. It is a different
// fact from a protocol failure or a closed session, and reporting it separately
// is what keeps "the CLI died" from arriving as an unexplained transport error.
// An exit status does not establish whether a request was billed.
type ProcessError struct {
	Engine   harness.Engine
	Code     string
	ExitCode *int
}

func (e *ProcessError) Error() string {
	out := string(e.Engine) + " harness "
	switch e.Code {
	case ProcessSignalled:
		out += "was terminated by a signal"
	case ProcessStartFailed:
		out += "could not be started"
	default:
		out += "exited"
	}
	if e.ExitCode != nil && *e.ExitCode >= 0 {
		out += " with status " + strconv.Itoa(*e.ExitCode)
	}
	return out
}

// Unwrap keeps ErrTransport identity, so existing callers that classify a lost
// harness as a transport failure continue to work while gaining the detail.
func (e *ProcessError) Unwrap() error { return ErrTransport }

// TurnError reports a native turn that the provider ended in failure. Code is a
// fixed value from the provider's own enumerated result vocabulary; provider
// prose is never placed in it. Bounded, sanitized detail reaches the caller
// through Options.OnDiagnostic instead, so a diagnostic can be recorded without
// a message that might be displayed carrying anything a provider wrote.
//
// Cause, and ResetsAt for an exhausted quota, are set where the harness said
// why the turn failed. In a session whose loop the library runs, a failed model
// request ends the turn with that request's own facts: Code, Cause, Phase,
// RetryAfter and ResetsAt are completion's RequestError fields.
type TurnError struct {
	Engine     harness.Engine
	Code       string
	Cause      harness.Cause
	Phase      string
	RetryAfter time.Duration
	ResetsAt   *time.Time
}

func (e *TurnError) Error() string {
	return string(e.Engine) + " harness turn failed: " + e.Code
}

// Unwrap keeps ErrTurnFailed identity, so callers that classified a failed turn
// before this existed keep working while gaining the code.
func (e *TurnError) Unwrap() error { return ErrTurnFailed }

func capabilityProofStep(step string) string {
	switch step {
	case sandbox.ProofStepFixture, sandbox.ProofStepOutside, sandbox.ProofStepLaunch, sandbox.ProofStepJudgment:
		return step
	}
	return ""
}

// RefusedLoopbackNotLocal identifies unprovable local-only loopback.
const RefusedLoopbackNotLocal = "loopback_local_only_unenforceable"
