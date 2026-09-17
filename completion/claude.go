package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ValidateClaudeHome accepts native or explicitly configured login storage.
// Custom instructions are disabled by --safe-mode, so existing CLAUDE.md files
// do not require copying credentials to a second account directory.
func ValidateClaudeHome(home string) error {
	if home == "" {
		return nil
	}
	if !filepath.IsAbs(home) || strings.ContainsRune(home, '\x00') {
		return preflightFailure("claude", "claude_home_invalid")
	}
	if stat, err := os.Stat(home); err == nil && !stat.IsDir() {
		return preflightFailure("claude", "claude_home_not_directory")
	} else if err != nil && !os.IsNotExist(err) {
		return preflightFailure("claude", "claude_home_unavailable")
	}
	return nil
}

func ClaudeEnvironment(home string) ([]string, error) {
	if err := ValidateClaudeHome(home); err != nil {
		return nil, err
	}
	// USER is part of native OS context: Claude needs it for macOS keychain
	// lookup. Windows native login/cache directories are retained explicitly too.
	env := append(nativeOperatingEnvironment(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "MAX_RETRIES=0")
	nativeHome, _ := os.UserHomeDir()
	if home != "" && filepath.Clean(home) != filepath.Join(nativeHome, ".claude") {
		env = append(env, "CLAUDE_CONFIG_DIR="+home)
	}
	// Auth is resolved natively by Claude (including keychain refresh). Never
	// forward API keys, integration secrets, or process-wide model overrides.
	return env, nil
}

func claudeBaseArgs() []string {
	return []string{"--safe-mode", "--setting-sources=", "--settings={\"disableAllHooks\":true}", "--strict-mcp-config", "--mcp-config={\"mcpServers\":{}}", "--tools=", "--disable-slash-commands", "--no-chrome", "--no-session-persistence", "--permission-mode", "dontAsk"}
}

func claudeComplete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Message, Usage, error) {
	var empty Message
	var usage Usage
	env, err := ClaudeEnvironment(cfg.ClaudeHome)
	if err != nil {
		return empty, usage, err
	}
	bin := cfg.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	bin, err = exec.LookPath(bin)
	if err != nil && cfg.run == nil {
		if failure := startFailure("claude", PhasePreflight, err); failure != nil {
			return empty, usage, failure
		}
		return empty, usage, preflightFailure("claude", "executable_not_found")
	}
	if cfg.run != nil && bin == "" {
		bin = cfg.ClaudeBin
	}
	if bin != "" {
		bin, err = filepath.Abs(bin)
		if err != nil {
			return empty, usage, preflightFailure("claude", "executable_unresolved")
		}
	}
	root := ""
	if cfg.WorkDirRoot != "" {
		root, err = ensureDirectory(cfg.WorkDirRoot, "model-runs")
		if err != nil {
			return empty, usage, preflightFailure("claude", "scratch_directory")
		}
	}
	dir, err := os.MkdirTemp(root, "agent-harness-claude-")
	if err != nil {
		return empty, usage, preflightFailure("claude", "scratch_directory")
	}
	defer os.RemoveAll(dir)
	schema, err := actionSchema(tools)
	if err != nil {
		return empty, usage, preflightFailure("claude", "invalid_tool_catalog")
	}
	payload, err := json.Marshal(map[string]any{"messages": messages, "available_tools": tools})
	if err != nil || len(payload) > cfg.MaxContextBytes {
		return empty, usage, &RequestError{Kind: ErrorContextLimit, Engine: "claude", Phase: PhasePreflight, Code: "context_bytes"}
	}
	args := append(claudeBaseArgs(), "-p", "--output-format", "stream-json", "--verbose", "--model", cfg.Model, "--system-prompt", codexInstructions, "--json-schema", string(schema))
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if err := probeClaude(ctx, cfg, bin, args, dir, env, schema); err != nil {
		return empty, usage, err
	}
	if err := ctx.Err(); err != nil {
		return empty, usage, err
	}
	if cfg.BeforeRequest != nil {
		if err := cfg.BeforeRequest(ctx); err != nil {
			return empty, usage, err
		}
	}
	output, err := runCLI(ctx, cfg, bin, args, dir, env, string(payload))
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return empty, usage, processRequestFailure("claude", output, err)
	}
	return parseClaude(output, tools)
}

func parseClaude(data []byte, tools []Tool) (Message, Usage, error) {
	if failure := claudeRequestFailure(data); failure != nil {
		return Message{}, Usage{}, failure
	}
	var usage Usage
	var result Message
	completed := false
	assistantError := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type    string   `json:"type"`
			Subtype string   `json:"subtype"`
			IsError bool     `json:"is_error"`
			Reason  string   `json:"terminal_reason"`
			Stop    string   `json:"stop_reason"`
			Error   string   `json:"error"`
			Tools   []string `json:"tools"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
			Structured json.RawMessage `json:"structured_output"`
			Usage      *struct {
				Input      int `json:"input_tokens"`
				Output     int `json:"output_tokens"`
				CacheRead  int `json:"cache_read_input_tokens"`
				CacheWrite int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &event) != nil {
			return Message{}, usage, &RequestError{Kind: ErrorUnknown, Engine: "claude", Phase: PhaseResponse, Code: "malformed_event_json"}
		}
		if event.Type == "system" && event.Subtype == "init" {
			for _, tool := range event.Tools {
				if tool != "StructuredOutput" {
					return Message{}, usage, &RequestError{Kind: ErrorUnknown, Engine: "claude", Phase: PhaseResponse, Code: "unexpected_native_tool"}
				}
			}
		}
		if event.Type == "assistant" {
			assistantError = claudeErrorCode(event.Error)
			for _, block := range event.Message.Content {
				if block.Type == "tool_use" && block.Name != "StructuredOutput" {
					return Message{}, usage, &RequestError{Kind: ErrorUnknown, Engine: "claude", Phase: PhaseResponse, Code: "unexpected_native_tool"}
				}
			}
		}
		if event.Type != "result" {
			continue
		}
		if event.IsError || event.Subtype != "success" || completed {
			return Message{}, usage, claudeTerminalFailure(event.Subtype, event.Reason, event.Stop, assistantError)
		}
		completed = true
		if event.Usage != nil {
			input := event.Usage.Input + event.Usage.CacheRead + event.Usage.CacheWrite
			usage = Usage{InputTokens: input, OutputTokens: event.Usage.Output, TotalTokens: input + event.Usage.Output, Known: true}
		}
		var err error
		result, err = parseActionEnvelope(event.Structured, tools)
		if err != nil {
			return Message{}, usage, &RequestError{Kind: ErrorUnknown, Engine: "claude", Phase: PhaseResponse, Code: "invalid_action_envelope"}
		}
	}
	if !completed {
		return Message{}, usage, &RequestError{Kind: ErrorUnknown, Engine: "claude", Phase: PhaseResponse, Code: "missing_terminal_result"}
	}
	return result, usage, nil
}
