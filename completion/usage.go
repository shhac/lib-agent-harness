package completion

import (
	"bytes"
	"encoding/json"
	"math"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/claudeproto"
)

// tokenReport is every token field either CLI's terminal report may carry.
// Pointers distinguish an absent field from a reported zero.
type tokenReport struct {
	Input  *int64 `json:"input_tokens"`
	Output *int64 `json:"output_tokens"`
	// Claude's cache split, reported beside input_tokens rather than in it.
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	// Codex's cached input and reasoning, reported as parts of input_tokens
	// and output_tokens.
	CachedInput     *int64 `json:"cached_input_tokens"`
	ReasoningOutput *int64 `json:"reasoning_output_tokens"`
	// Claude's thinking, reported as a part of output_tokens.
	OutputDetails *claudeproto.OutputDetails `json:"output_tokens_details"`
}

// terminalAccounting is the single definition of what a CLI stream establishes
// about consumption, used for successful and failed invocations alike so the
// two cannot disagree about the same provider's accounting. Its Message is
// always zero.
//
// Only an authoritative terminal report counts — Claude's `result` event or
// Codex's `turn.completed` — never a partial or streamed estimate. A stream
// that cannot be fully parsed, that carries more than one terminal report, or
// whose report is absent, incomplete, negative, inconsistent or too large to
// sum is unknown. A report of explicit zeros is a measurement and stays known.
func terminalAccounting(engine harness.Engine, data []byte) Result {
	var out Result
	terminals := 0
	servingModel := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type  string       `json:"type"`
			Usage *tokenReport `json:"usage"`
			// Raw because Codex uses these names for unrelated shapes (an error
			// event's message is a string); only Claude's are decoded.
			Message    json.RawMessage `json:"message"`
			ModelUsage json.RawMessage `json:"modelUsage"`
			// Raw so that an unreadable valuation leaves only the cost unknown.
			TotalCost json.RawMessage `json:"total_cost_usd"`
		}
		// A line we cannot read may be the terminal report, or may hide a second
		// one. Either way the stream is no longer authoritative about any of it.
		if json.Unmarshal(line, &event) != nil {
			return Result{}
		}
		if engine == harness.Claude && event.Type == "assistant" {
			if model := claudeMessageModel(event.Message); model != "" {
				servingModel = model
			}
		}
		if !terminalEvent(engine, event.Type) {
			continue
		}
		// Terminal reports are counted whether or not they carry accounting: a
		// second one makes the first ambiguous even when it is the one missing
		// its usage.
		terminals++
		if terminals > 1 {
			return Result{}
		}
		if engine == harness.Claude {
			out.ContextWindow = claudeContextWindow(event.ModelUsage, servingModel)
			out.Cost = claudeCost(event.TotalCost)
			out.Usage = claudeUsage(event.Usage)
			continue
		}
		out.Usage = codexUsage(event.Usage)
	}
	return out
}

// claudeUsage reads a result event's report. Claude states cache reads and
// writes beside input_tokens, so every prompt token is their sum. The split
// is known only when both cache fields are present; an absent one contributes
// nothing rather than making the report incomplete.
func claudeUsage(report *tokenReport) harness.Usage {
	if report == nil || report.Input == nil || report.Output == nil {
		return harness.Usage{}
	}
	cacheRead, cacheWrite := valueOrZero(report.CacheRead), valueOrZero(report.CacheWrite)
	input, ok := sumTokens(*report.Input, cacheRead, cacheWrite)
	if !ok {
		return harness.Usage{}
	}
	if _, ok := sumTokens(input, *report.Output); !ok {
		return harness.Usage{}
	}
	usage := harness.Usage{
		Known:      true,
		Input:      input,
		Output:     *report.Output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		CacheKnown: report.CacheRead != nil && report.CacheWrite != nil,
	}
	usage.Reasoning, usage.ReasoningKnown = report.OutputDetails.Reasoning(usage.Output)
	return usage
}

// codexUsage reads a turn.completed report. Codex's input_tokens already
// includes cached_input_tokens, and output_tokens includes
// reasoning_output_tokens; a part larger than its whole is inconsistent.
// Codex caching is implicit, so there is never a cache write to report.
func codexUsage(report *tokenReport) harness.Usage {
	if report == nil || report.Input == nil || report.Output == nil {
		return harness.Usage{}
	}
	if _, ok := sumTokens(*report.Input, *report.Output); !ok {
		return harness.Usage{}
	}
	usage := harness.Usage{Known: true, Input: *report.Input, Output: *report.Output}
	if cached := report.CachedInput; cached != nil {
		if *cached < 0 || *cached > usage.Input {
			return harness.Usage{}
		}
		usage.CacheRead, usage.CacheKnown = *cached, true
	}
	if reasoning := report.ReasoningOutput; reasoning != nil {
		if *reasoning < 0 || *reasoning > usage.Output {
			return harness.Usage{}
		}
		usage.Reasoning, usage.ReasoningKnown = *reasoning, true
	}
	return usage
}

// claudeCost is the valuation Claude states on its single terminal result. An
// absent, unreadable or negative figure is unknown, never free.
func claudeCost(raw json.RawMessage) harness.Cost {
	var usd *float64
	if len(raw) == 0 || json.Unmarshal(raw, &usd) != nil || usd == nil || *usd < 0 {
		return harness.Cost{}
	}
	return harness.Cost{USD: *usd, Known: true}
}

func claudeMessageModel(raw json.RawMessage) string {
	var message struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return ""
	}
	return message.Model
}

// claudeContextWindow reads the window modelUsage states for the model that
// served the request: the one the latest assistant message names, as the
// session package does. The requested name can be an alias the provider keyed
// differently, so a sole entry is that model when no named entry matches;
// several entries without a match leave the window unknown rather than chosen.
func claudeContextWindow(raw json.RawMessage, servingModel string) int64 {
	var models map[string]struct {
		ContextWindow *int64 `json:"contextWindow"`
	}
	if json.Unmarshal(raw, &models) != nil {
		return 0
	}
	entry, ok := models[servingModel]
	if !ok && len(models) == 1 {
		for _, sole := range models {
			entry, ok = sole, true
		}
	}
	if !ok || entry.ContextWindow == nil || *entry.ContextWindow <= 0 {
		return 0
	}
	return *entry.ContextWindow
}

func terminalEvent(engine harness.Engine, eventType string) bool {
	switch engine {
	case harness.Claude:
		return eventType == "result"
	case harness.Codex:
		return eventType == "turn.completed"
	}
	return false
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// sumTokens accepts counts only when each is non-negative and they sum without
// overflow. Anything else is unknown rather than a repaired number, because a
// repaired number cannot be told from a measured one once it is stored.
func sumTokens(parts ...int64) (int64, bool) {
	var total int64
	for _, part := range parts {
		if part < 0 || part > math.MaxInt64-total {
			return 0, false
		}
		total += part
	}
	return total, true
}
