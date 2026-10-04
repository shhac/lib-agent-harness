package sandbox

import (
	"errors"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
)

// ErrUnsupported identifies a refused sandbox operation.
var ErrUnsupported = errors.New("harness operation unsupported")

// ErrClosed identifies a workspace whose admission is closed.
var ErrClosed = errors.New("harness session closed")

// ErrCommandFailed identifies an unsettled workspace operation or a command failure.
// Its legacy turn-failure wording is retained for compatibility.
var ErrCommandFailed = errors.New("harness turn failed")

const (
	// RefusedWorkDir: the workspace directory could not be opened.
	RefusedWorkDir = "work_dir_unavailable"
	// RefusedWorkbenchMountCheck: workspace mount identity could not be checked.
	RefusedWorkbenchMountCheck = "workbench_mount_check_unavailable"
	// RefusedNotOffered: the platform does not offer a requested command operation.
	RefusedNotOffered = "not_offered"
	// CapabilitySandboxToolMissing: the required OS sandbox tool is absent.
	CapabilitySandboxToolMissing = "sandbox_tool_missing"
	// CapabilitySandboxToolOutdated: the installed sandbox tool is too old.
	CapabilitySandboxToolOutdated = "sandbox_tool_outdated"
	// CapabilitySandboxNamespacesUnavailable: required namespaces are unavailable.
	CapabilitySandboxNamespacesUnavailable = "sandbox_namespaces_unavailable"
	// CapabilitySandboxUnavailable: the OS sandbox proof could not run.
	CapabilitySandboxUnavailable = "sandbox_unavailable"
	// CapabilitySandboxNotEnforced: a canary escaped the requested boundary.
	CapabilitySandboxNotEnforced = "sandbox_not_enforced"
	// CapabilityProbeTimeout: the disposable proof timed out.
	CapabilityProbeTimeout = "probe_timed_out"
)

// RefusalError refuses workspace setup or a sandbox option before any work starts.
type RefusalError struct {
	Operation, Code string
	Capability      harness.Capability
}

func (e *RefusalError) Error() string { return e.Operation + ": " + e.Capability.Reason }
func (e *RefusalError) Unwrap() error { return ErrUnsupported }
func (e *RefusalError) HarnessFacts() harness.Facts {
	family := harness.FailurePreflight
	if e.Code == RefusedNotOffered || e.Code == RefusedConflict {
		family = harness.FailureCapability
	}
	return harness.Facts{Engine: harness.OpenAICompatible, Operation: harness.Session, Family: family, Code: e.Code}
}
func refusal(operation, code, reason string) *RefusalError {
	return &RefusalError{Operation: operation, Code: code, Capability: harness.Capability{Availability: harness.Unsupported, Reason: reason}}
}

// ProofError reports a failed pre-launch OS sandbox proof, without runtime output.
type ProofError struct {
	Code  string
	Tools []string
	Step  string
}

const (
	ProofStepFixture  = "fixture_preparation"
	ProofStepOutside  = "outside_control"
	ProofStepLaunch   = "sandbox_launch"
	ProofStepJudgment = "execution_judgment"
)

func proofStep(step string) string {
	switch step {
	case ProofStepFixture, ProofStepOutside, ProofStepLaunch, ProofStepJudgment:
		return step
	}
	return ""
}

func (e *ProofError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: harness.OpenAICompatible, Operation: harness.Session, Family: harness.FailureCapability, Phase: "before_launch", Code: e.Code, ProofStep: proofStep(e.Step)}
}
func (e *ProofError) Error() string {
	message := map[string]string{
		CapabilitySandboxToolMissing: "the required OS sandbox tool is missing; install a supported runtime",
		CapabilityProbeTimeout:       "the capability check did not finish in time",
		CapabilitySandboxUnavailable: "the installed harness could not be run under the requested sandbox",
		CapabilitySandboxNotEnforced: "the installed harness's sandbox allowed writes or network access the session must not have",
	}[e.Code]
	// Keep the legacy no-step wording for existing compatibility translations;
	// new execution proofs carry their specific step and expanded explanation.
	if e.Code == CapabilitySandboxNotEnforced && proofStep(e.Step) != "" {
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
	out := string(harness.OpenAICompatible) + ": " + message
	if len(e.Tools) > 0 {
		out += " (" + strings.Join(e.Tools, ", ") + ")"
	}
	if step := proofStep(e.Step); step != "" {
		out += "; proof step: " + step
	}
	return out + "; no session was started"
}
func (e *ProofError) Unwrap() error { return ErrUnsupported }

// CommandError reports unsettled workspace I/O or a command failure.
type CommandError struct{ Code string }

func (e *CommandError) Error() string {
	return string(harness.OpenAICompatible) + " harness turn failed: " + e.Code
}
func (e *CommandError) Unwrap() error { return ErrCommandFailed }
func (e *CommandError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: harness.OpenAICompatible, Operation: harness.Session, Family: harness.FailureTurn, Code: e.Code}
}

const (
	// CommandStartFailed: command launch failed.
	CommandStartFailed = "command_start_failed"
	// CommandOutcomeUnknown: command effects could not be established.
	CommandOutcomeUnknown = "command_outcome_unknown"
	// CommandProcessLimit: the command admission limit was reached.
	CommandProcessLimit = "command_process_limit"
	// CommandCleanupUnknown: command-tree cleanup could not be confirmed.
	CommandCleanupUnknown = "command_cleanup_unknown"
	// CommandSandboxClosed: command admission is closed.
	CommandSandboxClosed = "command_sandbox_closed"
)

// RefusedRuntimeHome: command recovery storage is unusable.
const RefusedRuntimeHome = "runtime_home_unusable"

// RefusedSandboxRead: the command read set is invalid.
const RefusedSandboxRead = "sandbox_read_path_invalid"

// RefusedLimit: a command option exceeds its allowed bounds.
const RefusedLimit = "limit_exceeded"

// RefusedConflict: command options conflict.
const RefusedConflict = "conflicting_options"

// ErrStateLocked identifies recovery state held by another live owner.
var ErrStateLocked = errors.New("another process holds this session's assignment lease")

// StateLocked: another owner holds the recovery directory lock.
const StateLocked = "session_locked"

// StateUnusable: recovery storage or ownership could not be established.
const StateUnusable = "state_unusable"

// StateError preserves uncertain recovery ownership without exposing paths.
type StateError struct{ Code string }

func (e *StateError) Error() string {
	if e.Code == StateLocked {
		return string(harness.OpenAICompatible) + ": the conversation is open in another session"
	}
	return string(harness.OpenAICompatible) + ": the session's runtime home cannot be used"
}
func (e *StateError) Unwrap() error {
	if e.Code == StateLocked {
		return ErrStateLocked
	}
	return nil
}
func (e *StateError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: harness.OpenAICompatible, Operation: harness.Session, Family: harness.FailurePreflight, Code: e.Code}
}
