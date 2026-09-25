package session

// The session's failure vocabulary: the sentinels a caller matches with
// errors.Is and the typed failures it inspects with errors.As. Codes are fixed
// library constants; no provider text, path or credential ever enters one.
// Failures that belong to one subsystem's protocol stay with it: the tool
// channel's in tools.go, and crash recovery's beside Reclaim.

import (
	"errors"
	"strconv"
	"strings"
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

type UnsupportedError struct {
	Operation  string
	Capability Capability
}

func (e *UnsupportedError) Error() string { return e.Operation + ": " + e.Capability.Reason }
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// errConversationGone marks a resume that failed because the harness no longer
// has the conversation, as opposed to one that failed for any other reason.
var errConversationGone = errors.New("harness conversation is unavailable to resume")

// Capability failure codes. They are fixed library constants: no provider text,
// path or credential ever enters one.
const (
	CapabilityUnsupportedPlatform = "restricted_session_unsupported_platform"
	CapabilityNativeToolsPresent  = "native_tools_present"
	CapabilityHostedToolsMissing  = "hosted_tools_missing"
	CapabilityInstructionsMerged  = "inherited_instructions_merged"
	CapabilityChangedModel        = "changed_model"
	CapabilityChangedEffort       = "changed_effort"
	CapabilityProbeNoRequest      = "probe_made_no_request"
	CapabilityProbeUnreadable     = "probe_request_unreadable"
	CapabilityProbeTimeout        = "probe_timed_out"
	CapabilityProbeFailed         = "probe_could_not_run"
	CapabilityCatalogUnavailable  = "model_catalog_unavailable"
	CapabilityCatalogRestriction  = "model_catalog_restriction_failed"
	CapabilityServerNotLoaded     = "tool_server_not_loaded"
	CapabilityServerNameReserved  = "tool_server_name_reserved"
	CapabilityLoginUnavailable    = "harness_login_unavailable"
)

// Sandbox capability failures.
const (
	// CapabilitySandboxUnavailable: the installed harness could not be put
	// under the requested sandbox, or the check could not be completed.
	CapabilitySandboxUnavailable = "sandbox_unavailable"
	// CapabilitySandboxNotEnforced: the sandbox the harness reported, or a
	// canary run under it, allowed something the session must not do.
	CapabilitySandboxNotEnforced = "sandbox_not_enforced"
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
)

// CapabilityError reports that a restricted session could not be established.
// Tool names, when present, are the ones the check disagreed about, and they
// come from the caller's own configuration or from a fixed native-name
// comparison — never from free text.
type CapabilityError struct {
	Engine string
	Code   string
	Phase  string
	Tools  []string
}

func (e *CapabilityError) Error() string {
	message := map[string]string{
		CapabilityUnsupportedPlatform: "restricted worker sessions are not available on this platform",
		CapabilityNativeToolsPresent:  "the installed harness kept tools this session did not configure",
		CapabilityHostedToolsMissing:  "the installed harness did not offer the tools this session configured",
		CapabilityInstructionsMerged:  "the installed harness merged inherited instructions into its request",
		CapabilityChangedModel:        "the installed harness requested a different model",
		CapabilityChangedEffort:       "the installed harness requested a different reasoning effort",
		CapabilityProbeNoRequest:      "the installed harness made no request during the capability check",
		CapabilityProbeUnreadable:     "the capability check could not read the harness's request",
		CapabilityProbeTimeout:        "the capability check did not finish in time",
		CapabilityProbeFailed:         "the capability check could not be run",
		CapabilityCatalogUnavailable:  "the installed harness did not supply a model catalog to restrict",
		CapabilityCatalogRestriction:  "the selected model could not be restricted in the installed harness catalog",
		CapabilityServerNotLoaded:     "the installed harness did not load this session's tool server",
		CapabilityServerNameReserved:  "the installed harness reserves this tool server name; choose another",
		CapabilityLoginUnavailable:    "the selected harness home has no file-backed login to share with a restricted session; log in to that home first",
		CapabilitySandboxUnavailable:  "the installed harness could not be run under the requested sandbox",
		CapabilitySandboxNotEnforced:  "the installed harness's sandbox allowed writes or network access the session must not have",
	}[e.Code]
	if message == "" {
		message = "the restricted session configuration could not be established"
	}
	out := e.Engine + ": " + message
	if len(e.Tools) > 0 {
		out += " (" + strings.Join(e.Tools, ", ") + ")"
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
	Engine   string
	Code     string
	ExitCode *int
}

func (e *ProcessError) Error() string {
	out := e.Engine + " harness "
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
type TurnError struct {
	Engine string
	Code   string
}

func (e *TurnError) Error() string {
	return e.Engine + " harness turn failed: " + e.Code
}

// Unwrap keeps ErrTurnFailed identity, so callers that classified a failed turn
// before this existed keep working while gaining the code.
func (e *TurnError) Unwrap() error { return ErrTurnFailed }
