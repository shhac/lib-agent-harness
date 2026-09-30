package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/claudeproto"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

// ValidateClaudeHome accepts native or explicitly configured login storage.
// Custom instructions are disabled by --safe-mode, so existing CLAUDE.md files
// do not require copying credentials to a second account directory.
func ValidateClaudeHome(home string) error {
	if code := nativecli.ClaudeHomeProblem(home); code != "" {
		return preflightFailure(harness.Claude, code)
	}
	return nil
}

// ClaudeEnvironment is the environment every Claude launch gets: the
// operating environment and the selected login home, and never an API key,
// integration secret or process-wide model override.
func ClaudeEnvironment(home string) ([]string, error) {
	env, code := nativecli.ClaudeEnvironment(home)
	if code != "" {
		return nil, preflightFailure(harness.Claude, code)
	}
	return env, nil
}

// claudeMaxOutputVariable is Claude Code's output cap. Only this adapter sets
// it, and only from Config; ClaudeEnvironment never forwards an ambient value.
const claudeMaxOutputVariable = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"

func claudeComplete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Result, error) {
	var empty Result
	env, err := ClaudeEnvironment(cfg.Provider.CLI.Home)
	if err != nil {
		return empty, err
	}
	if cfg.MaxOutputTokens > 0 {
		// Claude Code sets each request's max_tokens from this, lowered to the
		// model's own limit; the probe proves it reached the request.
		env = append(env, claudeMaxOutputVariable+"="+strconv.Itoa(cfg.MaxOutputTokens))
	}
	bin := cfg.Provider.CLI.Binary
	if bin == "" {
		bin = string(harness.Claude)
	}
	bin, err = exec.LookPath(bin)
	if err != nil && cfg.run == nil {
		if failure := startFailure(harness.Claude, PhasePreflight, err); failure != nil {
			return empty, failure
		}
		return empty, preflightFailure(harness.Claude, "executable_not_found")
	}
	if cfg.run != nil && bin == "" {
		bin = cfg.Provider.CLI.Binary
	}
	if bin != "" {
		bin, err = filepath.Abs(bin)
		if err != nil {
			return empty, preflightFailure(harness.Claude, "executable_unresolved")
		}
	}
	root := ""
	if cfg.WorkDirRoot != "" {
		root, err = ensureDirectory(cfg.WorkDirRoot, "model-runs")
		if err != nil {
			return empty, preflightFailure(harness.Claude, "scratch_directory")
		}
	}
	dir, err := os.MkdirTemp(root, "agent-harness-claude-")
	if err != nil {
		return empty, preflightFailure(harness.Claude, "scratch_directory")
	}
	defer os.RemoveAll(dir)
	schema, err := actionSchema(tools)
	if err != nil {
		return empty, preflightFailure(harness.Claude, "invalid_tool_catalog")
	}
	payload, err := json.Marshal(map[string]any{"messages": messages, "available_tools": tools})
	if err != nil || len(payload) > cfg.MaxContextBytes {
		return empty, &RequestError{Cause: harness.CauseContextLimit, Engine: harness.Claude, Phase: PhasePreflight, Code: "context_bytes"}
	}
	args := append(nativecli.ClaudeRestrictedArgs(), "-p", "--output-format", "stream-json", "--verbose", "--model", cfg.Model, "--system-prompt", codexInstructions, "--json-schema", string(schema))
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if err := probeClaude(ctx, cfg, bin, args, dir, env, schema); err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if cfg.BeforeRequest != nil {
		if err := cfg.BeforeRequest(ctx); err != nil {
			return empty, err
		}
	}
	output, err := runCLI(ctx, cfg, bin, args, dir, env, string(payload))
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		// A request that ended badly can still have been billed. Report whatever
		// the provider stated it consumed, and no action proposal.
		return terminalAccounting(harness.Claude, output), processRequestFailure(harness.Claude, output, err)
	}
	return parseClaude(output, tools)
}

func parseClaude(data []byte, tools []Tool) (Result, error) {
	accounting := terminalAccounting(harness.Claude, data)
	if failure, refusalOnly := claudeFailure(data); refusalOnly {
		return accounting, failure
	}
	var boundary claudeToolBoundary
	var message Message
	completed := false
	var refusal claudeproto.Refusal
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event claudeCompletionEvent
		if json.Unmarshal(line, &event) != nil {
			return accounting, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "malformed_event_json"}
		}
		if completed && (event.Type == "assistant" || event.Type == "user") {
			return accounting, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "unexpected_native_tool_call"}
		}
		if code := boundary.observe(event); code != "" {
			return accounting, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: code}
		}
		switch event.Type {
		case "assistant":
			refusal.Assistant(event.Error)
		case "rate_limit_event":
			refusal.RateLimit(event.Info, time.Now())
		}
		if event.Type != "result" {
			continue
		}
		if event.IsError || event.Subtype != "success" || completed {
			return accounting, claudeTerminalFailure(event.Subtype, event.Reason, event.Stop, refusal)
		}
		completed = true
		var err error
		message, err = parseActionEnvelope(event.Structured, tools)
		if err != nil {
			return accounting, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "invalid_action_envelope"}
		}
	}
	if len(boundary.pending) != 0 {
		return accounting, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "unexpected_native_tool_call"}
	}
	if !completed {
		return accounting, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Claude, Phase: PhaseResponse, Code: "missing_terminal_result"}
	}
	accounting.Message = message
	return accounting, nil
}
