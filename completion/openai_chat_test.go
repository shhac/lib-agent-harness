package completion

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

func TestOpenAIChatSendsTheConversationAsGiven(t *testing.T) {
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"hello"}`)))
	call := ToolCall{ID: "call_1", Type: "function"}
	call.Function.Name, call.Function.Arguments = "lookup", `{"q":"x"}`
	messages := []Message{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "Find x."},
		{Role: "assistant", ToolCalls: []ToolCall{call}},
		{Role: "tool", ToolCallID: "call_1", Content: `{"found":true}`},
	}
	tools := []Tool{
		{Type: "function", Function: Function{Name: "lookup", Description: "Look up.", Parameters: map[string]any{"type": "object"}, Strict: true}},
		{Type: "function", Function: Function{Name: "finish"}},
	}
	reply, usage, err := messageAndUsage(Complete(context.Background(), apiConfig(api), messages, tools))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Role != "assistant" || reply.Content != "hello" || reply.ToolCalls != nil {
		t.Fatalf("reply %#v", reply)
	}
	if usage != (harness.Usage{Known: true, Input: 5, Output: 2}) {
		t.Fatalf("usage %#v", usage)
	}
	request := api.last(t)
	if request.method != http.MethodPost || request.url != "https://gateway.invalid/v1/chat/completions" {
		t.Fatalf("%s %s", request.method, request.url)
	}
	for key, want := range map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "Accept": "application/json"} {
		if got := request.header.Get(key); got != want {
			t.Fatalf("%s = %q", key, got)
		}
	}
	// Only the model, conversation, catalog and a non-streaming flag are sent;
	// unset optional tool fields are omitted rather than sent as zero values.
	sameJSON(t, request.body, `{
		"model": "provider/test-model",
		"stream": false,
		"messages": [
			{"role": "system", "content": "Be brief."},
			{"role": "user", "content": "Find x."},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}]},
			{"role": "tool", "content": "{\"found\":true}", "tool_call_id": "call_1"}
		],
		"tools": [
			{"type": "function", "function": {"name": "lookup", "description": "Look up.", "parameters": {"type": "object"}, "strict": true}},
			{"type": "function", "function": {"name": "finish"}}
		]
	}`)
}

func TestOpenAIChatToolProposalsKeepProviderIDs(t *testing.T) {
	api := respondWith(200, chatBody(chatChoice(`"tool_calls"`,
		`{"role":"assistant","content":"Checking.","tool_calls":[`+chatCall("call_b", "lookup", `{"q":"b"}`)+`,`+chatCall("call_a", "finish", `{}`)+`]}`)))
	tools := append(append([]Tool(nil), lookupTool...), Tool{Type: "function", Function: Function{Name: "finish"}})
	reply, usage, err := messageAndUsage(Complete(context.Background(), apiConfig(api), userMessage, tools))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "Checking." || len(reply.ToolCalls) != 2 || !usage.Known {
		t.Fatalf("reply %#v usage %#v", reply, usage)
	}
	for i, want := range []struct{ id, name, arguments string }{{"call_b", "lookup", `{"q":"b"}`}, {"call_a", "finish", `{}`}} {
		call := reply.ToolCalls[i]
		if call.ID != want.id || call.Type != "function" || call.Function.Name != want.name || call.Function.Arguments != want.arguments {
			t.Fatalf("proposal %d: %#v", i, call)
		}
	}
}

// Gemini's OpenAI endpoint and older Ollama finish a tool-calling turn with
// "stop"; the calls are complete and are validated like any other.
func TestOpenAIChatStopWithToolCallsIsAProposal(t *testing.T) {
	api := respondWith(200, chatBody(chatChoice(`"stop"`, assistantWithCalls(chatCall("call_1", "lookup", `{"q":"x"}`)))))
	reply, _, err := messageAndUsage(Complete(context.Background(), apiConfig(api), userMessage, lookupTool))
	if err != nil || len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ID != "call_1" {
		t.Fatalf("reply %#v err %v", reply, err)
	}
}

func TestOpenAIChatSendsEffortOnlyWhereTheEndpointReadsIt(t *testing.T) {
	for parameter, want := range map[harness.EffortParameter]string{
		harness.EffortReasoningEffort: `{"model":"provider/test-model","stream":false,"messages":[{"role":"user","content":"secret prompt"}],"reasoning_effort":"high"}`,
		harness.EffortReasoningObject: `{"model":"provider/test-model","stream":false,"messages":[{"role":"user","content":"secret prompt"}],"reasoning":{"effort":"high"}}`,
	} {
		api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
		cfg := apiConfig(api)
		cfg.Effort, cfg.Provider.API.EffortParameter = "high", parameter
		if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, api.last(t).body, want)
	}
	// A parameter without an effort sends nothing.
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	cfg := apiConfig(api)
	cfg.Provider.API.EffortParameter = harness.EffortReasoningObject
	if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, api.last(t).body, `{"model":"provider/test-model","stream":false,"messages":[{"role":"user","content":"secret prompt"}]}`)
}

func TestOpenAIChatSendsAnEmptyParameterSchema(t *testing.T) {
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	tools := []Tool{{Type: "function", Function: Function{Name: "ping", Parameters: map[string]any{}}}}
	if _, err := Complete(context.Background(), apiConfig(api), userMessage, tools); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []struct {
			Function map[string]json.RawMessage `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(api.last(t).body, &body) != nil || string(body.Tools[0].Function["parameters"]) != "{}" {
		t.Fatalf("empty schema was dropped: %s", api.last(t).body)
	}
}

func TestOpenAIChatNullContentIsEmptyText(t *testing.T) {
	reply, _, err := messageAndUsage(Complete(context.Background(), apiConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":null}`)))), userMessage, nil))
	if err != nil || reply.Role != "assistant" || reply.Content != "" {
		t.Fatalf("%#v %v", reply, err)
	}
}

// Anything short of one unambiguous terminal choice yields no reply, but keeps
// the usage the provider reported, because the request may have been billed.
func TestOpenAIChatRejectsIncompleteOrAmbiguousResponses(t *testing.T) {
	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = chatCall(fmt.Sprintf("call_%d", i), "lookup", `{}`)
	}
	text := `{"role":"assistant","content":"secret partial"}`
	lookup := chatCall("call_1", "lookup", `{"q":"secret"}`)
	for _, tc := range []struct {
		name, body, code string
		usageKnown       bool
	}{
		{"stop with an invalid tool call", chatBody(chatChoice(`"stop"`, assistantWithCalls(chatCall("call_1", "secret_tool", `{}`)))), "invalid_tool_call", true},
		{"tool_calls without calls", chatBody(chatChoice(`"tool_calls"`, text)), "ambiguous_terminal_state", true},
		{"null finish", chatBody(chatChoice(`null`, text)), "ambiguous_terminal_state", true},
		{"missing finish", chatBody(`{"index":0,"message":` + text + `}`), "ambiguous_terminal_state", true},
		{"legacy function_call finish", chatBody(chatChoice(`"function_call"`, text)), "ambiguous_terminal_state", true},
		{"unknown finish", chatBody(chatChoice(`"secret_reason"`, text)), "ambiguous_terminal_state", true},
		{"truncated", chatBody(chatChoice(`"length"`, text)), "output_truncated", true},
		{"filtered", chatBody(chatChoice(`"content_filter"`, text)), "content_filtered", true},
		{"refusal", chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":null,"refusal":"secret refusal"}`)), "model_refusal", true},
		{"no choices", chatBody(), "unexpected_choice_count", true},
		{"two choices", chatBody(chatChoice(`"stop"`, text), chatChoice(`"stop"`, text)), "unexpected_choice_count", true},
		{"missing message", chatBody(`{"index":0,"finish_reason":"stop"}`), "malformed_response", true},
		{"non-assistant role", chatBody(chatChoice(`"stop"`, `{"role":"user","content":"secret"}`)), "malformed_response", true},
		{"content parts", chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":[{"type":"text","text":"secret"}]}`)), "malformed_response", true},
		{"object arguments", chatBody(chatChoice(`"tool_calls"`, `{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"lookup","arguments":{}}}]}`)), "malformed_response", true},
		{"trailing data", chatBody(chatChoice(`"stop"`, text)) + `{}`, "malformed_response", false},
		{"not JSON", `secret`, "malformed_response", false},
		{"unknown tool", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "secret_tool", `{}`)))), "invalid_tool_call", true},
		{"array arguments", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `[1]`)))), "invalid_tool_call", true},
		{"null arguments", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `null`)))), "invalid_tool_call", true},
		{"incomplete arguments", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `{"q":"secr`)))), "invalid_tool_call", true},
		{"duplicate IDs", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(lookup, lookup))), "invalid_tool_call", true},
		{"empty ID", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("", "lookup", `{}`)))), "invalid_tool_call", true},
		{"oversized ID", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall(strings.Repeat("x", apiToolCallIDLimit+1), "lookup", `{}`)))), "invalid_tool_call", true},
		{"non-function call", chatBody(chatChoice(`"tool_calls"`, `{"role":"assistant","tool_calls":[{"id":"c","type":"custom","function":{"name":"lookup","arguments":"{}"}}]}`)), "invalid_tool_call", true},
		{"missing function", chatBody(chatChoice(`"tool_calls"`, `{"role":"assistant","tool_calls":[{"id":"c","type":"function"}]}`)), "invalid_tool_call", true},
		{"too many calls", chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(seventeen...))), "invalid_tool_call", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, usage, err := messageAndUsage(Complete(context.Background(), apiConfig(respondWith(200, tc.body)), userMessage, lookupTool))
			failure := requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, tc.code)
			if failure.Retryable() || !reflect.DeepEqual(reply, Message{}) {
				t.Fatalf("rejected response leaked a reply or retry: %#v", reply)
			}
			if usage.Known != tc.usageKnown || (tc.usageKnown && usage.Total() != 7) {
				t.Fatalf("usage %#v", usage)
			}
		})
	}
	// With no catalog, any proposal is for an unavailable tool.
	_, err := Complete(context.Background(), apiConfig(respondWith(200, chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `{}`)))))), userMessage, nil)
	requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "invalid_tool_call")
}

func TestOpenAIChatUsageIsMeasuredOnlyFromCompleteReports(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        harness.Usage
	}{
		{"complete with cached detail", `"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}`, harness.Usage{Known: true, Input: 12, Output: 3, CacheRead: 4, CacheKnown: true}},
		{"explicit zero cached detail", `"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0}}`, harness.Usage{Known: true, Input: 12, Output: 3, CacheKnown: true}},
		{"reasoning detail", `"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21,"completion_tokens_details":{"reasoning_tokens":6}}`, harness.Usage{Known: true, Input: 12, Output: 9, Reasoning: 6}},
		// Many gateways omit the split; zero cache figures must then read as
		// unreported, not uncached.
		{"total absent", `"usage":{"prompt_tokens":5,"completion_tokens":2}`, harness.Usage{Known: true, Input: 5, Output: 2}},
		{"null details", `"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":null,"completion_tokens_details":null}`, harness.Usage{Known: true, Input: 5, Output: 2}},
		{"details without cached tokens", `"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"audio_tokens":0}}`, harness.Usage{Known: true, Input: 5, Output: 2}},
		{"null cached tokens", `"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":null}}`, harness.Usage{Known: true, Input: 5, Output: 2}},
		{"explicit zeros", `"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, harness.Usage{Known: true}},
		{"absent", `"system_fingerprint":"x"`, harness.Usage{}},
		{"null", `"usage":null`, harness.Usage{}},
		{"empty", `"usage":{}`, harness.Usage{}},
		{"completion missing", `"usage":{"prompt_tokens":5,"total_tokens":5}`, harness.Usage{}},
		{"negative", `"usage":{"prompt_tokens":-1,"completion_tokens":2}`, harness.Usage{}},
		{"inconsistent total", `"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":9}`, harness.Usage{}},
		{"cached exceeds prompt", `"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":6}}`, harness.Usage{}},
		{"negative cached", `"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":-1}}`, harness.Usage{}},
		{"reasoning exceeds completion", `"usage":{"prompt_tokens":5,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":3}}`, harness.Usage{}},
		{"overflow", fmt.Sprintf(`"usage":{"prompt_tokens":%d,"completion_tokens":1}`, math.MaxInt64), harness.Usage{}},
		{"non-numeric", `"usage":{"prompt_tokens":"5","completion_tokens":2}`, harness.Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"choices":[` + chatChoice(`"stop"`, `{"role":"assistant","content":"ok"}`) + `],` + tc.usage + `}`
			result, err := Complete(context.Background(), apiConfig(respondWith(200, body)), userMessage, nil)
			if err != nil || result.Message.Content != "ok" {
				t.Fatalf("an accounting gap must not discard the reply: %#v %v", result.Message, err)
			}
			if result.Usage != tc.want {
				t.Fatalf("usage %#v", result.Usage)
			}
			// Chat Completions states neither a valuation nor a window.
			if result.Cost != (harness.Cost{}) || result.ContextWindow != 0 {
				t.Fatalf("invented cost %#v or window %d", result.Cost, result.ContextWindow)
			}
		})
	}
}
