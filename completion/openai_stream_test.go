package completion

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func streamingConfig(api http.RoundTripper) Config {
	cfg := apiConfig(api)
	cfg.Provider.API.Streaming = true
	return cfg
}

// streamWith answers with a server-sent event stream.
func streamWith(events ...string) *fakeAPI {
	return &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
		body := strings.Join(events, "")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}}
}

func event(data string) string { return "data: " + data + "\n\n" }

const streamDone = "data: [DONE]\n\n"

func TestStreamedReplyAssemblesLikeAWholeOne(t *testing.T) {
	api := streamWith(
		": keepalive\n\n",
		event(`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"weigh"}}]}`),
		event(`{"choices":[{"index":0,"delta":{"reasoning_content":"ing"}}]}`),
		event(`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`),
		event(`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`),
		event(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`),
		streamDone,
	)
	result, err := Complete(context.Background(), streamingConfig(api), userMessage, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Content != "Hello" || !result.Usage.Known || result.Usage.Total() != 7 {
		t.Fatalf("%+v", result)
	}
	var replay struct {
		Fields map[string]string `json:"fields"`
	}
	if json.Unmarshal(result.Message.Replay, &replay) != nil || replay.Fields["reasoning_content"] != "weighing" {
		t.Fatalf("streamed reasoning was not kept: %s", result.Message.Replay)
	}
	var sent struct {
		Stream  bool `json:"stream"`
		Options struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(api.last(t).body, &sent) != nil || !sent.Stream || !sent.Options.IncludeUsage {
		t.Fatalf("request %s", api.last(t).body)
	}
	if accept := api.last(t).header.Get("Accept"); accept != "text/event-stream" {
		t.Fatalf("accept %q", accept)
	}
}

func TestStreamedToolCallsAssembleFromFragments(t *testing.T) {
	api := streamWith(
		event(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""},"extra_content":{"google":{"thought_signature":"sig"}}}]}}]}`),
		event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`),
		event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`),
		streamDone,
	)
	result, err := Complete(context.Background(), streamingConfig(api), userMessage, lookupTool)
	if err != nil {
		t.Fatal(err)
	}
	calls := result.Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Arguments != `{"q":"x"}` || len(calls[0].Replay) == 0 {
		t.Fatalf("%+v", calls)
	}
	// Usage was never reported, so it is unknown rather than zero.
	if result.Usage.Known {
		t.Fatalf("usage %+v", result.Usage)
	}
}

func TestStreamedFailuresAreTheSameFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		code   string
		cause  harness.Cause
	}{
		{"no terminator", []string{event(`{"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}]}`)}, "stream_incomplete", harness.CauseUnknown},
		{"no finish", []string{event(`{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`), streamDone}, "ambiguous_terminal_state", harness.CauseUnknown},
		{"cut off", []string{event(`{"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"length"}]}`), streamDone}, "output_truncated", harness.CauseOutputTruncated},
		{"incomplete arguments", []string{event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"lookup","arguments":"{\"q\""}}]},"finish_reason":"tool_calls"}]}`), streamDone}, "invalid_tool_call", harness.CauseUnknown},
		{"malformed chunk", []string{event(`secret`), streamDone}, "malformed_response", harness.CauseUnknown},
		{"second choice", []string{event(`{"choices":[{"index":1,"delta":{"content":"Hi"}}]}`), streamDone}, "unexpected_choice_count", harness.CauseUnknown},
		{"index out of bounds", []string{event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1000000000,"id":"c","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`), streamDone}, "invalid_tool_call", harness.CauseUnknown},
		{"negative index", []string{event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":-1,"id":"c"}]}}]}`), streamDone}, "invalid_tool_call", harness.CauseUnknown},
		{"missing index", []string{event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c"}]}}]}`), streamDone}, "invalid_tool_call", harness.CauseUnknown},
		{"second finish", []string{event(`{"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}]}`), event(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`), streamDone}, "ambiguous_terminal_state", harness.CauseUnknown},
		{"refused before any output", []string{event(`{"error":{"code":"insufficient_quota","message":"secret"}}`), streamDone}, "insufficient_quota", harness.CauseQuotaExhausted},
		{"streamed refusal", []string{event(`{"choices":[{"index":0,"delta":{"refusal":"secret"},"finish_reason":"stop"}]}`), streamDone}, "model_refusal", harness.CauseContentFiltered},
		{"failure mid-stream", []string{event(`{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`), event(`{"error":{"code":"insufficient_quota","message":"secret"}}`)}, "insufficient_quota", harness.CauseQuotaExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Complete(context.Background(), streamingConfig(streamWith(tc.events...)), userMessage, lookupTool)
			requireAPIFailure(t, err, PhaseResponse, tc.cause, tc.code)
			if result.Message.Content != "" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("a failed stream leaked a reply: %+v", result.Message)
			}
		})
	}
}

// A stalled stream is caught by its idle bound, well inside the request's
// own deadline.
func TestStalledStreamTimesOut(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { writer.Close() })
	api := &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
		go func() {
			_, _ = io.WriteString(writer, event(`{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`))
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: r}, nil
	}}
	cfg := streamingConfig(api)
	cfg.Provider.API.IdleTimeout = 50 * time.Millisecond
	started := time.Now()
	_, err := Complete(context.Background(), cfg, userMessage, nil)
	requireAPIFailure(t, err, PhaseResponse, harness.CauseTimeout, "stream_idle")
	if time.Since(started) > 5*time.Second {
		t.Fatal("the idle bound did not stop the request")
	}
}

// An endpoint that ignores the request to stream is read as a whole response.
func TestStreamingEndpointMayAnswerWhole(t *testing.T) {
	result, err := Complete(context.Background(), streamingConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"whole"}`)))), userMessage, nil)
	if err != nil || result.Message.Content != "whole" {
		t.Fatalf("%+v %v", result.Message, err)
	}
}

func TestIdleTimeoutRequiresStreaming(t *testing.T) {
	cfg := apiConfig(respondWith(200, chatBody()))
	cfg.Provider.API.IdleTimeout = time.Second
	if _, err := Complete(context.Background(), cfg, userMessage, nil); !hasCode(err, "api_idle_timeout_without_streaming") {
		t.Fatalf("%v", err)
	}
}

// OpenRouter streams reasoning_details a piece at a time; the reply keeps
// every piece, in order, to send back.
func TestStreamedReasoningDetailsAreKeptWhole(t *testing.T) {
	api := streamWith(
		event(`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"a"}]}}]}`),
		event(`{"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"b"}],"content":"ok"},"finish_reason":"stop"}]}`),
		streamDone,
	)
	result, err := Complete(context.Background(), streamingConfig(api), userMessage, nil)
	if err != nil {
		t.Fatal(err)
	}
	var replay struct {
		Fields struct {
			Details []struct{ Text string } `json:"reasoning_details"`
		} `json:"fields"`
	}
	if json.Unmarshal(result.Message.Replay, &replay) != nil || len(replay.Fields.Details) != 2 || replay.Fields.Details[0].Text != "a" || replay.Fields.Details[1].Text != "b" {
		t.Fatalf("reasoning details %s", result.Message.Replay)
	}
}
