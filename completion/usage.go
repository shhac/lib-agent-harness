package completion

import (
	"bytes"
	"encoding/json"
	"math"
)

// TerminalUsage recovers what a provider reported it consumed from a CLI stream
// that ended in a failure. A rejected or interrupted request can still have been
// billed, and dropping its accounting would present real consumption as free.
//
// It reads only an authoritative terminal report — Claude's `result` event or
// Codex's `turn.completed` — never a partial or streamed estimate, and never an
// action proposal. An absent, malformed, negative or overflowing report stays
// unknown: a caller must be able to tell unavailable accounting from zero.
func TerminalUsage(engine string, data []byte) Usage {
	if engine == "claude" {
		return terminalClaudeUsage(data)
	}
	if engine == "codex" {
		return terminalCodexUsage(data)
	}
	return Usage{}
}

func terminalClaudeUsage(data []byte) Usage {
	var out Usage
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
		if json.Unmarshal(line, &event) != nil || event.Type != "result" || event.Usage == nil {
			continue
		}
		// Cached input is input the provider charged for. Counting it keeps one
		// definition of consumption across engines and across success and failure.
		usage, ok := normalizedUsage(event.Usage.Input, event.Usage.Output, event.Usage.CacheRead, event.Usage.CacheWrite)
		if !ok {
			return Usage{}
		}
		// A stream carrying more than one terminal result is not authoritative
		// about any of them.
		if out.Known {
			return Usage{}
		}
		out = usage
	}
	return out
}

func terminalCodexUsage(data []byte) Usage {
	var out Usage
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Usage *struct {
				Input  *int `json:"input_tokens"`
				Output *int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "turn.completed" || event.Usage == nil {
			continue
		}
		usage, ok := normalizedUsage(event.Usage.Input, event.Usage.Output, nil, nil)
		if !ok {
			return Usage{}
		}
		if out.Known {
			return Usage{}
		}
		out = usage
	}
	return out
}

// normalizedUsage accepts a report only when every field it needs is present,
// non-negative and sums without overflow. Anything else is unknown rather than
// a repaired number, because a repaired number cannot be told from a measured
// one once it is stored.
func normalizedUsage(input, output *int, cacheRead, cacheWrite *int) (Usage, bool) {
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
	in := total - *output
	return Usage{InputTokens: in, OutputTokens: *output, TotalTokens: total, Known: true}, true
}
