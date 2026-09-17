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
		}
		// A line we cannot read may be the terminal report, or may hide a second
		// one. Either way the stream is no longer authoritative about any of it.
		if json.Unmarshal(line, &event) != nil {
			return Usage{}
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
		out = usage
	}
	return out
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
