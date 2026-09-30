package completion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

// A reasoning endpoint's reply, as DeepSeek's thinking mode and Gemini's
// OpenAI endpoint shape it: reasoning beside the message, and a thought
// signature on the call.
const reasoningReply = `{"role":"assistant","content":null,"reasoning_content":"thinking it through","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]}`

func TestProviderStateTravelsBackToItsEndpointOnly(t *testing.T) {
	first, err := Complete(context.Background(), apiConfig(respondWith(200, chatBody(chatChoice(`"tool_calls"`, reasoningReply)))), userMessage, lookupTool)
	if err != nil {
		t.Fatal(err)
	}
	reply := first.Message
	if len(reply.Replay) == 0 || len(reply.ToolCalls) != 1 || len(reply.ToolCalls[0].Replay) == 0 {
		t.Fatalf("provider state was dropped: %+v", reply)
	}
	history := append(append([]Message(nil), userMessage...), reply, Message{Role: "tool", ToolCallID: "call_1", Content: "found"})

	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"done"}`)))
	if _, err := Complete(context.Background(), apiConfig(api), history, lookupTool); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	body := api.last(t).body
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	assistant := sent.Messages[1]
	if string(assistant["reasoning_content"]) != `"thinking it through"` {
		t.Fatalf("reasoning was not sent back: %s", body)
	}
	var calls []map[string]json.RawMessage
	_ = json.Unmarshal(assistant["tool_calls"], &calls)
	if len(calls) != 1 || string(calls[0]["extra_content"]) != `{"google":{"thought_signature":"sig"}}` {
		t.Fatalf("the thought signature was not sent back: %s", body)
	}
	if strings.Contains(string(body), `"replay"`) {
		t.Fatalf("the library's envelope reached the wire: %s", body)
	}

	// Another model, another endpoint or a CLI engine could not use it, and
	// is not handed one provider's reasoning.
	elsewhere := apiConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"done"}`))))
	elsewhere.Model = "provider/other-model"
	if _, err := Complete(context.Background(), elsewhere, history, lookupTool); !hasCode(err, "replay_mismatch") {
		t.Fatalf("another model received the replay: %v", err)
	}
	cli := Config{Provider: harness.Provider{Engine: harness.Codex}, Model: "gpt-5.5"}
	if _, err := Complete(context.Background(), cli, history, lookupTool); !hasCode(err, "replay_mismatch") {
		t.Fatalf("a CLI engine was handed the replay: %v", err)
	}
	// A replay the caller built is not one this library made.
	forged := append([]Message(nil), history...)
	forged[1].Replay = json.RawMessage(`{"for":"x","fields":{"reasoning_content":"y"}}`)
	if _, err := Complete(context.Background(), apiConfig(api), forged, lookupTool); !hasCode(err, "replay_mismatch") {
		t.Fatalf("a foreign replay was sent: %v", err)
	}
}

func TestRepliesWithoutProviderStateCarryNone(t *testing.T) {
	result, err := Complete(context.Background(), apiConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"ok","reasoning_content":null}`)))), userMessage, nil)
	if err != nil || result.Message.Replay != nil {
		t.Fatalf("%+v %v", result.Message, err)
	}
}

func hasCode(err error, code string) bool {
	facts, ok := harness.ErrorFacts(err)
	return ok && facts.Code == code
}
