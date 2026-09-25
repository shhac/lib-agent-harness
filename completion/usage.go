package completion

import (
	"bytes"
	"encoding/json"
	"math"
)

// terminalUsage is the single definition of what a CLI stream establishes about
// consumption, used for successful and failed invocations alike so the two
// cannot disagree about the same provider's accounting.
//
// Only an authoritative terminal report counts — Claude's `result` event or
// Codex's `turn.completed` — never a partial or streamed estimate. A stream
// that cannot be fully parsed, that carries more than one terminal report, or
// whose report is absent, incomplete, negative or too large to sum is unknown.
// A report of explicit zeros is a measurement and stays known.
func terminalUsage(engine string, data []byte) Usage {
	var out Usage
	terminals := 0
	servingModel := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Usage *struct {
				Input      *int `json:"input_tokens"`
				Output     *int `json:"output_tokens"`
				CacheRead  *int `json:"cache_read_input_tokens"`
				CacheWrite *int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			// Raw because Codex uses these names for unrelated shapes (an error
			// event's message is a string); only Claude's are decoded.
			Message    json.RawMessage `json:"message"`
			ModelUsage json.RawMessage `json:"modelUsage"`
		}
		// A line we cannot read may be the terminal report, or may hide a second
		// one. Either way the stream is no longer authoritative about any of it.
		if json.Unmarshal(line, &event) != nil {
			return Usage{}
		}
		if engine == "claude" && event.Type == "assistant" {
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
			return Usage{}
		}
		if engine == "claude" {
			out.ContextWindow = claudeContextWindow(event.ModelUsage, servingModel)
		}
		if event.Usage == nil {
			continue
		}
		// Cached input is input the provider charged for. Counting it keeps one
		// definition of consumption across engines, and across success and
		// failure. Codex reports no cache split; its absent fields contribute
		// nothing rather than making the report incomplete.
		usage, ok := normalizedUsage(event.Usage.Input, event.Usage.Output, event.Usage.CacheRead, event.Usage.CacheWrite)
		if !ok {
			return Usage{}
		}
		usage.ContextWindow = out.ContextWindow
		out = usage
	}
	return out
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
func claudeContextWindow(raw json.RawMessage, servingModel string) int {
	var models map[string]struct {
		ContextWindow *int `json:"contextWindow"`
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

func terminalEvent(engine, eventType string) bool {
	if engine == "claude" {
		return eventType == "result"
	}
	return engine == "codex" && eventType == "turn.completed"
}

// normalizedUsage accepts a report only when the counts it needs are present,
// non-negative and sum without overflow. Anything else is unknown rather than a
// repaired number, because a repaired number cannot be told from a measured one
// once it is stored.
func normalizedUsage(input, output, cacheRead, cacheWrite *int) (Usage, bool) {
	if input == nil || output == nil {
		return Usage{}, false
	}
	parts := []int{*input, *output}
	if cacheRead != nil {
		parts = append(parts, *cacheRead)
	}
	if cacheWrite != nil {
		parts = append(parts, *cacheWrite)
	}
	total := 0
	for _, part := range parts {
		if part < 0 || part > math.MaxInt-total {
			return Usage{}, false
		}
		total += part
	}
	return Usage{InputTokens: total - *output, OutputTokens: *output, TotalTokens: total, Known: true}, true
}
