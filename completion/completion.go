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
	// MaxOutputTokens caps the tokens the model may generate for each model
	// request, reasoning included; zero leaves the provider's default and a
	// negative value is refused. Only an engine whose mechanism is proven
	// accepts it: Claude (checked in its capability probe) and an
	// OpenAI-compatible endpoint (max_completion_tokens). Codex and Grok
	// refuse it with max_output_tokens_unsupported rather than drop it. A
	// reply cut off at the cap is a failure, never a proposal.
	MaxOutputTokens int
	Timeout         time.Duration
	// BeforeRequest runs after non-billable probes and before inference.
	BeforeRequest func(context.Context) error
	// Skills are made available to the model through the library's skill
	// tools (see LoadSkillTool); installed skills are never loaded, so
	// Global Include is refused. Delivery is always composed here.
	Skills harness.Skills
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
	// SkillCalls are the calls in Message.ToolCalls to the library's skill
	// tools, in order. They stay in Message.ToolCalls so that the message can
	// be appended to history as it is; AnswerSkillCalls answers them, and
	// ApplicationCalls lists the rest.
	SkillCalls []ToolCall
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
	if harness.LoginStoreLocked(engine) {
		return Result{}, preflightFailure(engine, harness.CodeKeychainUnavailable)
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
	if cfg.MaxContextBytes < 1024 || cfg.Timeout <= 0 || cfg.MaxOutputTokens < 0 {
		return Result{}, preflightFailure(engine, "invalid_limits")
	}
	if cfg.MaxOutputTokens > 0 && !offersMaxOutputTokens(engine) {
		return Result{}, preflightFailure(engine, "max_output_tokens_unsupported")
	}
	messages, tools, err := composeSkills(engine, cfg.Skills, messages, tools)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result, err := dispatch(ctx, cfg, messages, tools)
	if err != nil || len(cfg.Skills.Provided) == 0 {
		return result, err
	}
	return separateSkillCalls(engine, result)
}

// offersMaxOutputTokens names the engines with a verified way to carry
// Config.MaxOutputTokens to the provider. Codex exec reads no output-token
// setting (codex-cli 0.156.1 sent none for any candidate key), and Grok's
// max_completion_tokens is overridden by a value its model catalog supplies,
// which no local probe can observe.
func offersMaxOutputTokens(engine harness.Engine) bool {
	return harness.Support(engine, harness.Complete, harness.MaxOutputTokens).Usable()
}

func dispatch(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Result, error) {
	switch engine := cfg.Provider.Engine; engine {
	case harness.Codex:
		return codexComplete(ctx, cfg, messages, tools)
	case harness.Claude:
		return claudeComplete(ctx, cfg, messages, tools)
	case harness.Grok:
		return grokComplete(ctx, cfg, messages, tools)
	case harness.OpenAICompatible:
		return openAIComplete(ctx, cfg, messages, tools)
	default:
		return Result{}, preflightFailure(engine, "unsupported_engine")
	}
}
