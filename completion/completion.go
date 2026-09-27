// Package completion runs CLI harnesses and remote API endpoints as constrained
// reasoning transports. CLI native tools are disabled and verified before
// inference. Returned tool calls are proposals only: authorization and
// execution remain with the caller.
package completion

import (
	"context"
	"net/http"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// Config selects a local CLI and its native login, or, with a
// harness.OpenAICompatible provider, an API endpoint and the caller's
// credential source. No credentials are copied, and ambient API keys are never
// read.
type Config struct {
	// Provider selects the engine. Codex and Claude read Provider.CLI; an
	// OpenAI-compatible engine reads Provider.API. Setting the other half is
	// refused rather than ignored.
	Provider harness.Provider
	Model    string
	Effort   string
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
	// transport replaces the API round trip for synthetic tests.
	transport http.RoundTripper
}

// Result is one invocation's reply and what the provider reported it consumed.
// A failed invocation that may have been billed returns a Result beside its
// error carrying that accounting, with a zero Message: a failure never
// carries an action proposal.
type Result struct {
	Message Message
	Usage   harness.Usage
	Cost    harness.Cost
	// ContextWindow is the provider's stated context window, in tokens, for the
	// model that served the request, taken from the same terminal report as the
	// usage (and so also present on a failed request that reported one). Zero
	// means unknown, never a guess. It is independent of Usage.Known: a report
	// can state the window without complete accounting, and the reverse.
	//
	// Claude states it in its result event's modelUsage. Codex exec reports no
	// window in its JSON event stream, and Chat Completions responses carry
	// none, so those requests always leave it zero.
	ContextWindow int64
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
func Complete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Result, error) {
	engine := cfg.Provider.Engine
	if !harness.Support(engine, harness.Complete, harness.Available).Usable() {
		return Result{}, preflightFailure(engine, "unsupported_engine")
	}
	if code := cfg.Provider.Problem(); code != "" {
		return Result{}, preflightFailure(engine, code)
	}
	if cfg.Model == "" {
		return Result{}, preflightFailure(engine, "model_required")
	}
	if cfg.MaxContextBytes == 0 {
		cfg.MaxContextBytes = 128 * 1024
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.MaxContextBytes < 1024 || cfg.Timeout <= 0 {
		return Result{}, preflightFailure(engine, "invalid_limits")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	switch engine {
	case harness.Codex:
		return codexComplete(ctx, cfg, messages, tools)
	case harness.Claude:
		return claudeComplete(ctx, cfg, messages, tools)
	case harness.Grok:
		return grokComplete(ctx, cfg, messages, tools)
	case harness.OpenAICompatible:
		return openAIComplete(ctx, cfg, messages, tools)
	}
	return Result{}, preflightFailure(engine, "unsupported_engine")
}
