package completion

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/shhac/lib-agent-harness"
)

// preflightFailure retains only library-owned codes, never a raw error cause.
func preflightFailure(engine harness.Engine, code string) *RequestError {
	if engine.Transport() == "" {
		engine = ""
	}
	cause := harness.CauseUnknown
	if code == "probe_timeout" || code == "catalog_timeout" {
		cause = harness.CauseTimeout
	}
	return &RequestError{Cause: cause, Engine: engine, Phase: PhasePreflight, Code: code}
}

func (e *RequestError) diagnosticDetail() string {
	switch e.Code {
	case "unexpected_native_tool_catalog":
		if e.Engine == harness.Grok {
			return "Grok advertised native tools during constrained completion and was stopped; no application actions were accepted"
		}
		return "Claude advertised native tools during constrained completion; check CLI tool isolation before resuming"
	case "unexpected_native_tool_call":
		if e.Engine == harness.Grok {
			return "Grok attempted a native tool call during constrained completion and was stopped; no application actions were accepted"
		}
		return "Claude emitted a native tool call without a verified unavailable-tool rejection; no application actions were accepted"
	case "unexpected_native_instructions":
		return "Grok's persisted transcript showed instructions the capability check did not prove; the reply was discarded"
	case "missing_native_tool_catalog":
		return "Grok did not state an empty native tool catalog; the reply was discarded"
	case "missing_native_transcript":
		return "Grok's persisted transcript could not be read to verify its instructions; the reply was discarded"
	case "missing_structured_output":
		return "Grok ended without structured output"
	case "max_tokens", "max_turn_requests":
		return "Grok stopped at its output or turn limit before finishing; no proposal was returned"
	case "refusal":
		return "Grok's model refused the request; no proposal was returned"
	case "cancelled":
		return "Grok cancelled the turn before finishing; no proposal was returned"
	case "unexpected_stop_reason", "end_error":
		return "Grok ended the turn without finishing; no proposal was returned"
	case "grok_error":
		return "Grok reported a request failure; outcome or usage may be unknown"
	case "grok_platform_unsupported":
		return "Grok constrained completion is unavailable on this platform: its private runtime home needs owner-only files"
	case "work_dir_root_required":
		return "Grok constrained completion requires WorkDirRoot, the private state directory its runtime home lives in"
	case "grok_home_unresolved":
		return "cannot resolve Grok login home"
	case "grok_home_invalid":
		return "Grok home must be an absolute directory path"
	case "grok_home_is_runtime":
		return "Grok login home must be separate from the runtime home under WorkDirRoot"
	case "grok_login_unavailable":
		return "Grok login home has no file-backed login (auth.json); run grok and sign in with that home"
	case "grok_login_unreadable":
		return "Grok login could not be read as a credential file; check the login home"
	case "grok_runtime_home":
		return "cannot prepare Grok's private runtime home under WorkDirRoot"
	case "grok_version_unavailable":
		return "cannot read the installed Grok version; check the CLI installation (no account inference was attempted)"
	case "invalid_effort":
		return "reasoning effort cannot be passed to the CLI as given"
	case "probe_missing_tool_catalog":
		return "CLI capability check saw no empty native tool catalog; constrained completion remains disabled (not an account login check)"
	case "probe_transcript_unverified":
		return "CLI capability check could not verify the transcript the CLI persists; constrained completion remains disabled (not an account login check)"
	case "unexpected_native_tool":
		return "Claude advertised or attempted an unexpected native tool; this older diagnostic does not distinguish the two"
	case "global_skills_invalid":
		return "Skills.Global must be Default, Include or Exclude"
	case "global_skills_unsupported":
		return "constrained completion never loads installed skills; use Skills.Global Default or Exclude and provide the skills needed"
	case "skill_delivery_invalid":
		return "Skills.Delivery must be Auto or Composed"
	case "skills_unsupported":
		return "provided skills are unavailable for this engine here"
	case "skill_tool_name_reserved":
		return "load_skill and run_skill_script are reserved for the library's skill tools while skills are provided; rename the application tool"
	case "invalid_skill_call":
		return "the model proposed a skill tool call with malformed arguments; no proposal was returned"
	case "too_many_skills":
		return "too many provided skills for one invocation"
	case "skill_name_invalid":
		return "a skill name must be lowercase letters, digits and hyphens, at most 64 characters"
	case "skill_duplicate":
		return "two provided skills share a name"
	case "skill_dir_invalid":
		return "a skill directory must be an absolute path"
	case "skill_dir_unavailable":
		return "a skill directory does not exist or is not a directory"
	case "skill_manifest_missing":
		return "a skill directory has no SKILL.md"
	case "skill_manifest_invalid":
		return "a skill's SKILL.md must be a regular UTF-8 text file inside its directory"
	case "skill_manifest_too_large":
		return "a skill's SKILL.md exceeds 256 KiB"
	case "skill_front_matter_invalid":
		return "a skill's SKILL.md must open with YAML front matter giving name and description as simple scalars"
	case "skill_name_mismatch":
		return "a skill's SKILL.md names a different skill than the one provided"
	case "skill_description_invalid":
		return "a skill's description must be non-empty text of at most 1024 characters"

	case "executable_not_found":
		if e.Engine == harness.Claude {
			return "Claude executable not found; install Claude Code and sign in"
		}
		if e.Engine == harness.Grok {
			return "Grok executable not found; install Grok and sign in"
		}
		return "Codex executable not found; install Codex and run codex login"
	case "unsupported_engine":
		return "unsupported engine; use codex, claude, grok or openai-compatible"
	case "api_config_for_cli_engine":
		return "a CLI engine reads Provider.CLI; clear Provider.API"
	case "cli_config_for_api_engine":
		return "an API engine reads Provider.API; clear Provider.CLI"
	case "model_required":
		return "model is required"
	case "invalid_limits":
		return "invalid context, timeout or output token limit; MaxOutputTokens must not be negative"
	case "max_output_tokens_unsupported":
		return "MaxOutputTokens has no verified mechanism for this engine; leave it zero or use Claude or an OpenAI-compatible endpoint"
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
	case "probe_changed_max_output_tokens":
		return "CLI capability check found the output token cap missing or exceeded; constrained completion remains disabled (not an account login check)"
	case "probe_mismatch":
		return "CLI capability check failed to prove tool-free inference; check installed CLI compatibility (no account inference was attempted)"

	case "api_dialect_required":
		return "API dialect is required; select harness.OpenAIChatCompletions explicitly"
	case "api_dialect_unsupported":
		return "unsupported API dialect; harness.OpenAIChatCompletions is the only supported dialect"
	case "api_base_url_invalid":
		return "API base URL must be an absolute https URL without user information, query or fragment"
	case "api_base_url_insecure":
		return "API base URL must use https unless it names a loopback host"
	case "api_credentials_required":
		return "API credential source is required; ambient API keys are never read (set Unauthenticated only for a local server that takes no credential)"
	case "api_credentials_conflict":
		return "API config sets both a credential source and Unauthenticated; choose one"
	case "api_unauthenticated_remote":
		return "Unauthenticated is allowed only for a loopback API base URL"
	case "api_effort_parameter_required":
		return "reasoning effort needs API.EffortParameter: harness.EffortReasoningEffort (OpenAI, xAI) or harness.EffortReasoningObject (Vercel AI Gateway, OpenRouter)"
	case "api_effort_parameter_unsupported":
		return "unsupported API.EffortParameter; use harness.EffortReasoningEffort or harness.EffortReasoningObject"
	case "api_effort_invalid":
		return "reasoning effort must be a short lowercase level such as low, medium or high"
	case "invalid_messages":
		return "invalid conversation; use system, user, assistant and tool roles, with tool call IDs only where the role allows them"
	case "credential_unavailable":
		return "API credential source failed; check the application's credential provider (no request was sent)"
	case "invalid_credential":
		return "API credential source returned an empty or malformed bearer token (no request was sent)"
	case "redirect_refused":
		return "API endpoint redirected the request, which is never followed; configure the final endpoint URL"
	case "credential_echoed":
		return "API endpoint echoed the request credential; the response was discarded"
	}
	return ""
}

// startFailure inspects only OS error identity and type; paths and error text
// are never retained. Exit errors are handled by the response parser instead.
func startFailure(engine harness.Engine, phase ErrorPhase, err error) *RequestError {
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
	return &RequestError{Cause: harness.CauseUnknown, Engine: engine, Phase: phase, Code: code}
}

func probeRunFailure(engine harness.Engine, ctx context.Context, err error) error {
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
