package completion

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

// preflightFailure retains only library-owned codes, never a raw error cause.
func preflightFailure(engine, code string) *RequestError {
	if engine != "claude" && engine != "codex" {
		engine = ""
	}
	kind := ErrorUnknown
	if code == "probe_timeout" || code == "catalog_timeout" {
		kind = ErrorTimeout
	}
	return &RequestError{Kind: kind, Engine: engine, Phase: PhasePreflight, Code: code}
}

func (e *RequestError) diagnosticDetail() string {
	switch e.Code {
	case "executable_not_found":
		if e.Engine == "claude" {
			return "Claude executable not found; install Claude Code and sign in"
		}
		return "Codex executable not found; install Codex and run codex login"
	case "unsupported_engine":
		return "unsupported CLI harness"
	case "model_required":
		return "model is required"
	case "invalid_limits":
		return "invalid context or timeout limit"
	case "executable_unresolved":
		return "cannot resolve CLI executable; check the configured executable path"
	case "scratch_directory":
		return "cannot prepare model scratch directory; check that the configured state root is a writable real directory"
	case "scratch_write":
		return "cannot write model scratch files; check available disk space and directory permissions"
	case "invalid_tool_catalog":
		return "invalid application tool catalog; provide uniquely named function tools"
	case "invalid_model_catalog":
		return "Codex returned an invalid model catalog; upgrade the CLI and retry"
	case "missing_effort_catalog":
		return "Codex model has no reasoning-effort catalog; choose a model advertised by the installed CLI"
	case "unsupported_effort":
		return "Codex model does not advertise the selected reasoning effort; choose an effort from its model catalog"
	case "catalog_timeout":
		return "Codex bundled model catalog read timed out; check CLI startup and retry (no account inference was attempted)"
	case "catalog_read_failed":
		return "cannot read Codex bundled model catalog; upgrade Codex to a CLI supporting debug models --bundled"
	case "codex_home_unresolved":
		return "cannot resolve Codex login home"
	case "codex_home_invalid":
		return "Codex home must be an absolute directory path"
	case "codex_home_unavailable":
		return "Configured Codex home is unavailable; sign in with the selected Codex home before starting this runtime"
	case "codex_home_inspection":
		return "cannot inspect Codex instruction boundary"
	case "codex_home_instructions":
		return "Codex home contains global AGENTS instructions that exec cannot disable; choose a dedicated Codex home containing no AGENTS.md or AGENTS.override.md and sign in with that home (credentials are not copied)"
	case "claude_home_invalid":
		return "Claude home must be an absolute directory"
	case "claude_home_not_directory":
		return "Claude home must be a directory"
	case "claude_home_unavailable":
		return "cannot access Claude home"
	case "executable_permission":
		return "cannot execute CLI; check executable permissions and the configured path"
	case "executable_relative":
		return "CLI executable resolved relative to the current directory; configure an absolute executable path"
	case "working_directory_unavailable":
		return "cannot enter CLI working directory; check that the scratch directory exists and is accessible"
	case "process_start_failed":
		return "cannot start CLI; check its installation and the configured executable and working directories"
	case "probe_listen_failed":
		return "cannot start local capability check; allow loopback connections and retry"
	case "probe_timeout":
		return "CLI capability check timed out against the local test provider; check CLI startup and supported flags (no account inference was attempted)"
	case "probe_no_requests":
		return "CLI capability check made no request to the local test provider; check CLI startup and supported flags (no account inference attempted)"
	case "probe_request_limit":
		return "CLI capability check exceeded its local request limit; no account inference was attempted"
	case "probe_invalid_request":
		return "CLI capability check rejected invalid or oversized request; constrained completion remains disabled (not an account login check)"
	case "probe_unexpected_tools":
		return "CLI capability check rejected unexpected tools; constrained completion remains disabled (not an account login check)"
	case "probe_invalid_schema":
		return "CLI capability check rejected invalid output schema; constrained completion remains disabled (not an account login check)"
	case "probe_changed_schema":
		return "CLI capability check rejected changed output schema; constrained completion remains disabled (not an account login check)"
	case "probe_changed_effort":
		return "CLI capability check rejected changed reasoning effort; constrained completion remains disabled (not an account login check)"
	case "probe_instruction_type":
		return "CLI capability check rejected unexpected system instruction type; constrained completion remains disabled (not an account login check)"
	case "probe_unexpected_instructions":
		return "CLI capability check rejected unexpected system instructions; constrained completion remains disabled (not an account login check)"
	case "probe_missing_instructions":
		return "CLI capability check rejected missing application instructions; constrained completion remains disabled (not an account login check)"
	case "probe_changed_model":
		return "CLI capability check rejected changed model; constrained completion remains disabled (not an account login check)"
	case "probe_mismatch":
		return "CLI capability check failed to prove tool-free inference; check installed CLI compatibility (no account inference was attempted)"
	}
	return ""
}

// startFailure inspects only OS error identity and type; paths and error text
// are never retained. Exit errors are handled by the response parser instead.
func startFailure(engine string, phase ErrorPhase, err error) *RequestError {
	code := ""
	var pathError *os.PathError
	switch {
	case errors.As(err, &pathError) && pathError.Op == "chdir":
		code = "working_directory_unavailable"
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		code = "executable_not_found"
	case errors.Is(err, os.ErrPermission):
		code = "executable_permission"
	case errors.Is(err, exec.ErrDot):
		code = "executable_relative"
	default:
		var execError *exec.Error
		if errors.As(err, &pathError) || errors.As(err, &execError) {
			code = "process_start_failed"
		}
	}
	if code == "" {
		return nil
	}
	return &RequestError{Kind: ErrorUnknown, Engine: engine, Phase: phase, Code: code}
}

func probeRunFailure(engine string, ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return preflightFailure(engine, "probe_timeout")
	}
	if failure := startFailure(engine, PhasePreflight, err); failure != nil {
		return failure
	}
	return nil
}
