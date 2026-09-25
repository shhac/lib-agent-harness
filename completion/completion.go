// Package completion runs CLI harnesses as constrained reasoning transports.
// Native tools are disabled and verified before inference. Returned tool calls
// are proposals only: authorization and execution remain with the caller.
package completion

import (
	"context"
	"time"
)

// Config selects a local CLI and its native login. No API credentials are copied.
type Config struct {
	Engine     string
	Model      string
	Effort     string
	CodexBin   string
	CodexHome  string
	ClaudeBin  string
	ClaudeHome string
	// WorkDirRoot is a canonical existing private directory, never a project directory.
	// On Windows it must already have a private ACL: children inherit its ACL,
	// and chmod does not establish an owner-only Windows access policy.
	WorkDirRoot     string
	MaxContextBytes int
	Timeout         time.Duration
	// BeforeRequest runs after non-billable probes and before inference.
	BeforeRequest func(context.Context) error
	// run replaces CLI execution for synthetic tests, for either engine.
	run func(context.Context, string, []string, string, []string, string) ([]byte, error)
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type Usage struct {
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	TotalTokens  int  `json:"total_tokens"`
	Known        bool `json:"known"`
	// ContextWindow is the provider's stated context window, in tokens, for the
	// model that served the request, taken from the same terminal report as the
	// token counts (and so also present on a failed request that reported one).
	// Zero means unknown, never a guess. It is independent of Known: a report
	// can state the window without complete accounting, and the reverse.
	//
	// Claude states it in its result event's modelUsage. Codex exec reports no
	// window in its JSON event stream, so Codex requests always leave it zero.
	ContextWindow int `json:"context_window,omitempty"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// Complete performs one invocation and never executes proposed application tools.
func Complete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Message, Usage, error) {
	if cfg.Engine != "codex" && cfg.Engine != "claude" {
		return Message{}, Usage{}, preflightFailure(cfg.Engine, "unsupported_engine")
	}
	if cfg.Model == "" {
		return Message{}, Usage{}, preflightFailure(cfg.Engine, "model_required")
	}
	if cfg.MaxContextBytes == 0 {
		cfg.MaxContextBytes = 128 * 1024
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.MaxContextBytes < 1024 || cfg.Timeout <= 0 {
		return Message{}, Usage{}, preflightFailure(cfg.Engine, "invalid_limits")
	}
	if err := ctx.Err(); err != nil {
		return Message{}, Usage{}, err
	}
	if cfg.Engine == "claude" {
		return claudeComplete(ctx, cfg, messages, tools)
	}
	return codexComplete(ctx, cfg, messages, tools)
}
