package session

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// raw replies with a response body exactly as given.
func raw(body string) endpointReply {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// A reasoning endpoint needs its reasoning and thought signatures back with
// the history, in the turn that produced them and after a resume alike.
func TestAPISessionSendsProviderStateBackAcrossAResume(t *testing.T) {
	e := newEndpoint(t,
		raw(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"reasoning_content":"weighing it","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"google":{"thought_signature":"sig"}}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		answer("Read it."),
	)
	var mu sync.Mutex
	var calls []ToolCall
	o := apiOptions(t, e.url, echo(&calls, &mu))
	s := startAPI(t, o)
	ref := s.Ref()
	if done := runAPITurnToEnd(t, s, "Read the file."); done.err != nil {
		t.Fatal(done.err)
	}
	closeAPI(t, s)

	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI(t, resumed)
	e.script(answer("Still here."))
	if done := runAPITurnToEnd(t, resumed, "Anything else?"); done.err != nil {
		t.Fatal(done.err)
	}
	for i, request := range e.seen()[1:] {
		assistant := request.Messages[2]
		if assistant.Role != "assistant" || string(assistant.ReasoningContent) != `"weighing it"` || len(assistant.ToolCalls) != 1 || !strings.Contains(string(assistant.ToolCalls[0]), `"extra_content":{"google":{"thought_signature":"sig"}}`) {
			t.Fatalf("request %d lost the provider state: %+v", i+1, assistant)
		}
		if strings.Contains(string(assistant.ToolCalls[0]), `"replay"`) {
			t.Fatalf("the library's envelope reached the wire: %s", assistant.ToolCalls[0])
		}
	}
}
