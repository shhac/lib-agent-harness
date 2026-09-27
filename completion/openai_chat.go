package completion

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/shhac/lib-agent-harness"
)

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []chatMessage  `json:"messages"`
	Tools           []chatTool     `json:"tools,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Reasoning       *chatReasoning `json:"reasoning,omitempty"`
	// MaxCompletionTokens is OpenAI's current output cap. The deprecated
	// max_tokens is not sent: OpenAI's reasoning models do not accept it.
	MaxCompletionTokens int  `json:"max_completion_tokens,omitempty"`
	Stream              bool `json:"stream"`
}

type chatReasoning struct {
	Effort string `json:"effort"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is null only for an assistant message that carries tool calls.
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is any rather than a map so that an empty schema the caller
	// supplied is sent as {} while an absent one is omitted.
	Parameters any  `json:"parameters,omitempty"`
	Strict     bool `json:"strict,omitempty"`
}

func chatRequestBody(cfg Config, messages []Message, tools []Tool) ([]byte, error) {
	if _, err := toolCatalog(tools); err != nil {
		return nil, preflightFailure(harness.OpenAICompatible, "invalid_tool_catalog")
	}
	wire, ok := chatMessages(messages)
	if !ok {
		return nil, preflightFailure(harness.OpenAICompatible, "invalid_messages")
	}
	request := chatRequest{Model: cfg.Model, Messages: wire, MaxCompletionTokens: cfg.MaxOutputTokens}
	switch cfg.Provider.API.EffortParameter {
	case harness.EffortReasoningEffort:
		request.ReasoningEffort = cfg.Effort
	case harness.EffortReasoningObject:
		if cfg.Effort != "" {
			request.Reasoning = &chatReasoning{Effort: cfg.Effort}
		}
	}
	for _, tool := range tools {
		function := chatFunction{Name: tool.Function.Name, Description: tool.Function.Description, Strict: tool.Function.Strict}
		if tool.Function.Parameters != nil {
			function.Parameters = tool.Function.Parameters
		}
		request.Tools = append(request.Tools, chatTool{Type: "function", Function: function})
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, preflightFailure(harness.OpenAICompatible, "invalid_tool_catalog")
	}
	return body, nil
}

func chatMessages(messages []Message) ([]chatMessage, bool) {
	if len(messages) == 0 {
		return nil, false
	}
	wire := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		content := message.Content
		out := chatMessage{Role: message.Role, Content: &content, ToolCallID: message.ToolCallID}
		switch message.Role {
		case "system", "user":
			if len(message.ToolCalls) > 0 || message.ToolCallID != "" {
				return nil, false
			}
		case "tool":
			if len(message.ToolCalls) > 0 || message.ToolCallID == "" {
				return nil, false
			}
		case "assistant":
			if message.ToolCallID != "" {
				return nil, false
			}
			for _, call := range message.ToolCalls {
				if call.Type != "function" || call.ID == "" || call.Function.Name == "" {
					return nil, false
				}
			}
			out.ToolCalls = message.ToolCalls
			if len(message.ToolCalls) > 0 && content == "" {
				out.Content = nil
			}
		default:
			return nil, false
		}
		wire = append(wire, out)
	}
	return wire, true
}

type chatCompletionResponse struct {
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Message      *struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Refusal   *string         `json:"refusal"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// parseChatCompletion accepts only an unambiguous terminal response. Anything
// else returns no reply, alongside the usage the response reported.
func parseChatCompletion(data []byte, tools []Tool) (Result, error) {
	accounting := Result{Usage: chatUsage(data)}
	var response chatCompletionResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF {
		return accounting, apiResponseFailure("malformed_response")
	}
	if len(response.Choices) != 1 {
		return accounting, apiResponseFailure("unexpected_choice_count")
	}
	choice := response.Choices[0]
	message := choice.Message
	if message == nil || (message.Role != "" && message.Role != "assistant") {
		return accounting, apiResponseFailure("malformed_response")
	}
	if message.Refusal != nil && *message.Refusal != "" {
		return accounting, apiResponseFailure("model_refusal")
	}
	result := Message{Role: "assistant"}
	if len(message.Content) > 0 && string(message.Content) != "null" && json.Unmarshal(message.Content, &result.Content) != nil {
		return accounting, apiResponseFailure("malformed_response")
	}
	if code := finishFailure(choice.FinishReason, len(message.ToolCalls)); code != "" {
		return accounting, apiResponseFailure(code)
	}
	if len(message.ToolCalls) > maxToolProposals {
		return accounting, apiResponseFailure("invalid_tool_call")
	}
	allowed, _ := toolCatalog(tools)
	seen := map[string]bool{}
	for _, call := range message.ToolCalls {
		if call.Type != "function" || call.Function == nil || call.ID == "" || len(call.ID) > apiToolCallIDLimit || seen[call.ID] || !allowed[call.Function.Name] || !jsonObject(call.Function.Arguments) {
			return accounting, apiResponseFailure("invalid_tool_call")
		}
		seen[call.ID] = true
		proposal := ToolCall{ID: call.ID, Type: "function"}
		proposal.Function.Name = call.Function.Name
		proposal.Function.Arguments = call.Function.Arguments
		result.ToolCalls = append(result.ToolCalls, proposal)
	}
	accounting.Message = result
	return accounting, nil
}

// finishFailure accepts "stop" with tool calls as well as "tool_calls": some
// compatible providers (Gemini's OpenAI endpoint, older Ollama) report a
// completed tool-calling turn as "stop". Neither is truncation, and every call
// is still validated before it is proposed.
func finishFailure(reason *string, calls int) string {
	if reason == nil {
		return "ambiguous_terminal_state"
	}
	switch {
	case *reason == "stop", *reason == "tool_calls" && calls > 0:
		return ""
	case *reason == "length":
		return "output_truncated"
	case *reason == "content_filter":
		return "content_filtered"
	}
	return "ambiguous_terminal_state"
}

func jsonObject(arguments string) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(arguments), &object) == nil && object != nil
}

// chatUsage applies the CLI adapters' rule: a report counts only when complete
// and consistent. prompt_tokens already includes cached input, and
// completion_tokens includes reasoning; many compatible endpoints omit either
// detail, so the cache split is known only when cached_tokens is present.
func chatUsage(data []byte) harness.Usage {
	var response struct {
		Usage *struct {
			Prompt        *int64 `json:"prompt_tokens"`
			Completion    *int64 `json:"completion_tokens"`
			Total         *int64 `json:"total_tokens"`
			PromptDetails *struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails *struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil || response.Usage == nil {
		return harness.Usage{}
	}
	report := response.Usage
	if report.Prompt == nil || report.Completion == nil {
		return harness.Usage{}
	}
	total, ok := sumTokens(*report.Prompt, *report.Completion)
	if !ok || (report.Total != nil && *report.Total != total) {
		return harness.Usage{}
	}
	usage := harness.Usage{Known: true, Input: *report.Prompt, Output: *report.Completion}
	if report.PromptDetails != nil && report.PromptDetails.Cached != nil {
		cached := *report.PromptDetails.Cached
		if cached < 0 || cached > usage.Input {
			return harness.Usage{}
		}
		usage.CacheRead, usage.CacheKnown = cached, true
	}
	if report.CompletionDetails != nil && report.CompletionDetails.Reasoning != nil {
		reasoning := *report.CompletionDetails.Reasoning
		if reasoning < 0 || reasoning > usage.Output {
			return harness.Usage{}
		}
		usage.Reasoning = reasoning
	}
	return usage
}
