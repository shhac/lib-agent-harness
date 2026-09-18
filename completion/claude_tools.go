package completion

import "encoding/json"

type claudeCompletionEvent struct {
	Type    string   `json:"type"`
	Subtype string   `json:"subtype"`
	IsError bool     `json:"is_error"`
	Reason  string   `json:"terminal_reason"`
	Stop    string   `json:"stop_reason"`
	Error   string   `json:"error"`
	Tools   []string `json:"tools"`
	Message struct {
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`
	Structured json.RawMessage `json:"structured_output"`
}
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// An unavailable tool may be hallucinated even with --tools=, then rejected by
// the CLI before StructuredOutput succeeds. Only that explicit no-execution
// receipt is recoverable. A generic tool error could follow partial effects.
// This validates the CLI's stream, not model-authored text inside a response.
type claudeToolBoundary struct {
	initialized bool
	seen        map[string]bool
	settled     map[string]bool
	pending     map[string]string
}

func (b *claudeToolBoundary) observe(e claudeCompletionEvent) string {
	if e.Type == "system" && e.Subtype == "init" {
		if b.initialized {
			return "unexpected_native_tool_catalog"
		}
		b.initialized = true
		for _, name := range e.Tools {
			if name != "StructuredOutput" {
				return "unexpected_native_tool_catalog"
			}
		}
	}
	if e.Type == "assistant" {
		for _, block := range e.Message.Content {
			if block.Type != "tool_use" {
				continue
			}
			if block.ID != "" {
				if b.seen[block.ID] {
					return "unexpected_native_tool_call"
				}
				if b.seen == nil {
					b.seen = map[string]bool{}
				}
				b.seen[block.ID] = true
			}
			if block.Name == "StructuredOutput" {
				continue
			}
			if !b.initialized || block.Name == "" || block.ID == "" {
				return "unexpected_native_tool_call"
			}
			if b.pending == nil {
				b.pending = map[string]string{}
			}
			b.pending[block.ID] = block.Name
		}
	}
	if e.Type == "user" {
		for _, block := range e.Message.Content {
			if block.Type != "tool_result" {
				continue
			}
			if !b.seen[block.ToolUseID] || b.settled[block.ToolUseID] {
				return "unexpected_native_tool_call"
			}
			if b.settled == nil {
				b.settled = map[string]bool{}
			}
			b.settled[block.ToolUseID] = true
			name, pending := b.pending[block.ToolUseID]
			if !pending {
				continue
			}
			var content string
			if !block.IsError || json.Unmarshal(block.Content, &content) != nil || content != "<tool_use_error>Error: No such tool available: "+name+"</tool_use_error>" {
				return "unexpected_native_tool_call"
			}
			delete(b.pending, block.ToolUseID)
		}
	}
	if e.Type == "result" && len(b.pending) != 0 {
		return "unexpected_native_tool_call"
	}
	return ""
}
