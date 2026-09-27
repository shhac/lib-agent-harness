package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// A dummy credential. It contains "secret" so leak checks can find it.
const testToken = "secret-dummy-token-not-real"

type recordedRequest struct {
	method, url string
	header      http.Header
	body        []byte
}

// fakeAPI stands in for every network round trip; no test reaches a network.
type fakeAPI struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(*http.Request) (*http.Response, error)
}

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{method: r.Method, url: r.URL.String(), header: r.Header.Clone(), body: body})
	f.mu.Unlock()
	return f.respond(r)
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAPI) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("want exactly one request, got %d", len(f.requests))
	}
	return f.requests[0]
}

func respondWith(status int, body string, header ...string) *fakeAPI {
	return &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
		for i := 0; i+1 < len(header); i += 2 {
			response.Header.Set(header[i], header[i+1])
		}
		return response, nil
	}}
}

func apiConfig(api http.RoundTripper) Config {
	return Config{
		Engine: EngineOpenAICompatible,
		Model:  "provider/test-model",
		API: APIConfig{
			BaseURL:     "https://gateway.invalid/v1",
			Dialect:     OpenAIChatCompletions,
			Credentials: func(context.Context) (string, error) { return testToken, nil },
		},
		transport: api,
	}
}

var userMessage = []Message{{Role: "user", Content: "secret prompt"}}

var lookupTool = []Tool{{Type: "function", Function: Function{Name: "lookup"}}}

const testUsage = `"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`

func chatBody(choices ...string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","choices":[` + strings.Join(choices, ",") + `],` + testUsage + `}`
}

func chatChoice(finish, message string) string {
	return `{"index":0,"finish_reason":` + finish + `,"message":` + message + `}`
}

func chatCall(id, name, arguments string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, id, name, arguments)
}

func assistantWithCalls(calls ...string) string {
	return `{"role":"assistant","content":null,"tool_calls":[` + strings.Join(calls, ",") + `]}`
}

func requireAPIFailure(t *testing.T, err error, phase ErrorPhase, kind ErrorKind, code string) *RequestError {
	t.Helper()
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Engine != EngineOpenAICompatible || failure.Phase != phase || failure.Kind != kind || failure.Code != code {
		t.Fatalf("want %s/%s/%s; got %#v (%v)", phase, kind, code, failure, err)
	}
	encoded, _ := json.Marshal(failure)
	if strings.Contains(string(encoded)+fmt.Sprintf("%#v", failure)+err.Error(), "secret") {
		t.Fatalf("failure retained provider text or credential: %v", err)
	}
	return failure
}

func sameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("request body\n got: %s\nwant: %s", got, want)
	}
}

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
	reply, usage, err := Complete(context.Background(), apiConfig(api), messages, tools)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Role != "assistant" || reply.Content != "hello" || reply.ToolCalls != nil {
		t.Fatalf("reply %#v", reply)
	}
	if usage != (Usage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7, Known: true}) {
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

func TestOpenAIChatEndpointAndEmptyCatalog(t *testing.T) {
	for base, want := range map[string]string{
		"https://gateway.invalid/v1/":      "https://gateway.invalid/v1/chat/completions",
		"https://gateway.invalid":          "https://gateway.invalid/chat/completions",
		"http://127.0.0.1:8080/v1":         "http://127.0.0.1:8080/v1/chat/completions",
		"http://localhost/openai/v1":       "http://localhost/openai/v1/chat/completions",
		"http://[::1]:9000/v1":             "http://[::1]:9000/v1/chat/completions",
		"https://gateway.invalid/v1/a%2Fb": "https://gateway.invalid/v1/a%2Fb/chat/completions",
	} {
		api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"ok"}`)))
		cfg := apiConfig(api)
		cfg.API.BaseURL = base
		if _, _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		request := api.last(t)
		if request.url != want {
			t.Fatalf("%s: requested %s", base, request.url)
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(request.body, &body) != nil || body["tools"] != nil {
			t.Fatalf("empty catalog sent tools: %s", request.body)
		}
	}
}

func TestOpenAIChatToolProposalsKeepProviderIDs(t *testing.T) {
	api := respondWith(200, chatBody(chatChoice(`"tool_calls"`,
		`{"role":"assistant","content":"Checking.","tool_calls":[`+chatCall("call_b", "lookup", `{"q":"b"}`)+`,`+chatCall("call_a", "finish", `{}`)+`]}`)))
	tools := append(append([]Tool(nil), lookupTool...), Tool{Type: "function", Function: Function{Name: "finish"}})
	reply, usage, err := Complete(context.Background(), apiConfig(api), userMessage, tools)
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

func TestOpenAIChatNullContentIsEmptyText(t *testing.T) {
	reply, _, err := Complete(context.Background(), apiConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":null}`)))), userMessage, nil)
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
		{"stop with tool calls", chatBody(chatChoice(`"stop"`, assistantWithCalls(lookup))), "ambiguous_terminal_state", true},
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
			reply, usage, err := Complete(context.Background(), apiConfig(respondWith(200, tc.body)), userMessage, lookupTool)
			failure := requireAPIFailure(t, err, PhaseResponse, ErrorUnknown, tc.code)
			if failure.Retryable() || !reflect.DeepEqual(reply, Message{}) {
				t.Fatalf("rejected response leaked a reply or retry: %#v", reply)
			}
			if usage.Known != tc.usageKnown || (tc.usageKnown && usage.TotalTokens != 7) {
				t.Fatalf("usage %#v", usage)
			}
		})
	}
	// With no catalog, any proposal is for an unavailable tool.
	_, _, err := Complete(context.Background(), apiConfig(respondWith(200, chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `{}`)))))), userMessage, nil)
	requireAPIFailure(t, err, PhaseResponse, ErrorUnknown, "invalid_tool_call")
}

func TestOpenAIChatUsageIsMeasuredOnlyFromCompleteReports(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        Usage
	}{
		{"complete with cached detail", `"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}`, Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, Known: true}},
		{"total absent", `"usage":{"prompt_tokens":5,"completion_tokens":2}`, Usage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7, Known: true}},
		{"explicit zeros", `"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, Usage{Known: true}},
		{"absent", `"system_fingerprint":"x"`, Usage{}},
		{"null", `"usage":null`, Usage{}},
		{"empty", `"usage":{}`, Usage{}},
		{"completion missing", `"usage":{"prompt_tokens":5,"total_tokens":5}`, Usage{}},
		{"negative", `"usage":{"prompt_tokens":-1,"completion_tokens":2}`, Usage{}},
		{"inconsistent total", `"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":9}`, Usage{}},
		{"overflow", fmt.Sprintf(`"usage":{"prompt_tokens":%d,"completion_tokens":1}`, math.MaxInt), Usage{}},
		{"non-numeric", `"usage":{"prompt_tokens":"5","completion_tokens":2}`, Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"choices":[` + chatChoice(`"stop"`, `{"role":"assistant","content":"ok"}`) + `],` + tc.usage + `}`
			reply, usage, err := Complete(context.Background(), apiConfig(respondWith(200, body)), userMessage, nil)
			if err != nil || reply.Content != "ok" {
				t.Fatalf("an accounting gap must not discard the reply: %#v %v", reply, err)
			}
			if usage != tc.want {
				t.Fatalf("usage %#v", usage)
			}
		})
	}
}

func TestOpenAIChatHTTPFailuresAreClassifiedWithoutProviderText(t *testing.T) {
	errorBody := func(code, kind string) string {
		return `{"error":{"message":"secret provider text ` + testToken + `","type":"` + kind + `","code":"` + code + `"}}`
	}
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		header     []string
		kind       ErrorKind
		code       string
		retryAfter time.Duration
	}{
		{"unauthorized", 401, errorBody("invalid_api_key", "invalid_request_error"), nil, ErrorAuthentication, "http_401", 0},
		{"forbidden", 403, errorBody("", "secret"), nil, ErrorPermissionDenied, "http_403", 0},
		{"model not found", 404, errorBody("model_not_found", "invalid_request_error"), nil, ErrorModelUnavailable, "model_not_found", 0},
		{"wrong path", 404, `secret not found`, nil, ErrorUnknown, "http_404", 0},
		{"context length", 400, errorBody("context_length_exceeded", "invalid_request_error"), nil, ErrorContextLimit, "context_length_exceeded", 0},
		{"bad request", 400, errorBody("secret_code", "invalid_request_error"), nil, ErrorUnknown, "http_400", 0},
		{"too large", 413, ``, nil, ErrorContextLimit, "http_413", 0},
		{"rate limited", 429, errorBody("rate_limit_exceeded", "requests"), []string{"Retry-After", "7"}, ErrorRateLimited, "http_429", 7 * time.Second},
		{"rate limited unreadable body", 429, `secret`, nil, ErrorRateLimited, "http_429", 0},
		{"rate limited date delay", 429, ``, []string{"Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT"}, ErrorRateLimited, "http_429", 0},
		{"rate limited excessive delay", 429, ``, []string{"Retry-After", "7200"}, ErrorRateLimited, "http_429", 0},
		{"quota exhausted", 429, errorBody("insufficient_quota", "insufficient_quota"), []string{"Retry-After", "7"}, ErrorUnknown, "insufficient_quota", 0},
		{"quota exhausted by type", 429, errorBody("", "insufficient_quota"), nil, ErrorUnknown, "insufficient_quota", 0},
		{"unavailable", 503, ``, []string{"Retry-After", "3"}, ErrorUnavailable, "http_503", 3 * time.Second},
		{"overloaded", 529, errorBody("", "overloaded_error"), nil, ErrorOverloaded, "http_529", 0},
		{"server error", 500, errorBody("", "server_error"), []string{"Retry-After", "3"}, ErrorUnknown, "http_500", 0},
		{"bad gateway", 502, ``, nil, ErrorUnknown, "http_502", 0},
		{"gateway timeout", 504, ``, nil, ErrorUnknown, "http_504", 0},
		{"unexpected success status", 201, chatBody(chatChoice(`"stop"`, `{"content":"secret"}`)), nil, ErrorUnknown, "http_201", 0},
		{"redirect", 307, ``, []string{"Location", "https://elsewhere.invalid/v1/chat/completions"}, ErrorUnknown, "redirect_refused", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(tc.status, tc.body, tc.header...)
			reply, usage, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
			failure := requireAPIFailure(t, err, PhaseResponse, tc.kind, tc.code)
			if failure.RetryAfter != tc.retryAfter {
				t.Fatalf("retry after %v", failure.RetryAfter)
			}
			wantRetry := tc.kind == ErrorRateLimited || tc.kind == ErrorUnavailable || tc.kind == ErrorOverloaded
			if failure.Retryable() != wantRetry {
				t.Fatalf("retryable %v", failure.Retryable())
			}
			if !reflect.DeepEqual(reply, Message{}) || usage != (Usage{}) {
				t.Fatalf("failed status produced %#v %#v", reply, usage)
			}
			if api.count() != 1 {
				t.Fatalf("made %d requests; redirects and retries are never followed", api.count())
			}
		})
	}
	var failure *RequestError
	_, _, err := Complete(context.Background(), apiConfig(respondWith(404, errorBody("model_not_found", ""))), userMessage, nil)
	if !errors.As(err, &failure) || !strings.Contains(err.Error(), "configured endpoint") {
		t.Fatalf("model message should describe the endpoint, not a CLI: %v", err)
	}
}

func TestOpenAIChatRejectsNonJSONAndOversizedResponses(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "text/html", ""} {
		api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"secret"}`)), "Content-Type", contentType)
		if contentType == "" {
			api = &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			}}
		}
		_, _, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
		requireAPIFailure(t, err, PhaseResponse, ErrorUnknown, "unexpected_media_type")
	}
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"`+strings.Repeat("x", apiResponseLimit)+`"}`)), "Content-Type", "application/json; charset=utf-8")
	_, _, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
	requireAPIFailure(t, err, PhaseResponse, ErrorUnknown, "output_limit")
}

func TestOpenAIChatDiscardsAResponseEchoingTheCredential(t *testing.T) {
	escaped := `s` + strings.TrimPrefix(testToken, "s")
	for name, body := range map[string]string{
		"raw":            chatBody(chatChoice(`"stop"`, `{"content":"Bearer `+testToken+`"}`)),
		"escaped text":   chatBody(chatChoice(`"stop"`, `{"content":"`+escaped+`"}`)),
		"tool arguments": chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `{"k":"`+testToken+`"}`)))),
	} {
		t.Run(name, func(t *testing.T) {
			reply, usage, err := Complete(context.Background(), apiConfig(respondWith(200, body)), userMessage, lookupTool)
			requireAPIFailure(t, err, PhaseResponse, ErrorUnknown, "credential_echoed")
			if !reflect.DeepEqual(reply, Message{}) || !usage.Known {
				t.Fatalf("reply %#v usage %#v", reply, usage)
			}
		})
	}
}

// Refusals happen before BeforeRequest, before the credential source is asked,
// and before any request is built.
func TestOpenAIChatPreflightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config, *[]Message, *[]Tool)
		code   string
	}{
		{"dialect required", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.Dialect = "" }, "api_dialect_required"},
		{"responses dialect not yet", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.Dialect = "openai-responses" }, "api_dialect_unsupported"},
		{"empty URL", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "" }, "api_base_url_invalid"},
		{"relative URL", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "/secret/v1" }, "api_base_url_invalid"},
		{"other scheme", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "ftp://gateway.invalid/v1" }, "api_base_url_invalid"},
		{"user information", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "https://user:secret@gateway.invalid/v1" }, "api_base_url_invalid"},
		{"query", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "https://gateway.invalid/v1?key=secret" }, "api_base_url_invalid"},
		{"fragment", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "https://gateway.invalid/v1#secret" }, "api_base_url_invalid"},
		{"plaintext remote", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "http://gateway.invalid/v1" }, "api_base_url_insecure"},
		{"plaintext private network", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.BaseURL = "http://10.0.0.1/v1" }, "api_base_url_insecure"},
		{"credentials required", func(c *Config, _ *[]Message, _ *[]Tool) { c.API.Credentials = nil }, "api_credentials_required"},
		{"effort", func(c *Config, _ *[]Message, _ *[]Tool) { c.Effort = "low" }, "api_effort_unsupported"},
		{"no messages", func(_ *Config, m *[]Message, _ *[]Tool) { *m = nil }, "invalid_messages"},
		{"unknown role", func(_ *Config, m *[]Message, _ *[]Tool) { *m = []Message{{Role: "secret", Content: "x"}} }, "invalid_messages"},
		{"tool result without ID", func(_ *Config, m *[]Message, _ *[]Tool) { *m = []Message{{Role: "tool", Content: "x"}} }, "invalid_messages"},
		{"user with tool ID", func(_ *Config, m *[]Message, _ *[]Tool) { *m = []Message{{Role: "user", ToolCallID: "c"}} }, "invalid_messages"},
		{"user with tool calls", func(_ *Config, m *[]Message, _ *[]Tool) {
			*m = []Message{{Role: "user", ToolCalls: []ToolCall{{ID: "c", Type: "function"}}}}
		}, "invalid_messages"},
		{"assistant call without ID", func(_ *Config, m *[]Message, _ *[]Tool) {
			call := ToolCall{Type: "function"}
			call.Function.Name = "lookup"
			*m = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}}
		}, "invalid_messages"},
		{"duplicate tool", func(_ *Config, _ *[]Message, tools *[]Tool) { *tools = append(lookupTool, lookupTool...) }, "invalid_tool_catalog"},
		{"non-function tool", func(_ *Config, _ *[]Message, tools *[]Tool) { *tools = []Tool{{Type: "secret"}} }, "invalid_tool_catalog"},
		{"unencodable parameters", func(_ *Config, _ *[]Message, tools *[]Tool) {
			*tools = []Tool{{Type: "function", Function: Function{Name: "lookup", Parameters: map[string]any{"x": func() {}}}}}
		}, "invalid_tool_catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
			cfg := apiConfig(api)
			asked := false
			if cfg.API.Credentials != nil {
				cfg.API.Credentials = func(context.Context) (string, error) { asked = true; return testToken, nil }
			}
			cfg.BeforeRequest = func(context.Context) error { t.Fatal("refused request reached BeforeRequest"); return nil }
			messages, tools := userMessage, []Tool(nil)
			tc.mutate(&cfg, &messages, &tools)
			_, _, err := Complete(context.Background(), cfg, messages, tools)
			requireDiagnostic(t, err, EngineOpenAICompatible, PhasePreflight, tc.code)
			if asked || api.count() != 0 {
				t.Fatal("refused request resolved a credential or reached the transport")
			}
		})
	}
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	cfg := apiConfig(api)
	cfg.MaxContextBytes = 1024
	_, _, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: strings.Repeat("secret", 200)}}, nil)
	requireAPIFailure(t, err, PhasePreflight, ErrorContextLimit, "context_bytes")
	if api.count() != 0 {
		t.Fatal("oversized request was sent")
	}
}

func TestOpenAIChatCredentialSource(t *testing.T) {
	t.Run("resolved once, after BeforeRequest, under the request deadline", func(t *testing.T) {
		var order []string
		cfg := apiConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`))))
		cfg.BeforeRequest = func(context.Context) error { order = append(order, "before"); return nil }
		cfg.API.Credentials = func(ctx context.Context) (string, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("credential source is not bounded by Timeout")
			}
			order = append(order, "credential")
			return testToken, nil
		}
		if _, _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Join(order, ",") != "before,credential" {
			t.Fatalf("order %v", order)
		}
	})
	t.Run("a refusing BeforeRequest mints no credential", func(t *testing.T) {
		api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
		cfg := apiConfig(api)
		gate := errors.New("budget exhausted")
		cfg.BeforeRequest = func(context.Context) error { return gate }
		cfg.API.Credentials = func(context.Context) (string, error) { t.Fatal("credential resolved"); return "", nil }
		if _, _, err := Complete(context.Background(), cfg, userMessage, nil); !errors.Is(err, gate) || api.count() != 0 {
			t.Fatalf("%v, %d requests", err, api.count())
		}
	})
	for _, tc := range []struct {
		name, token, code string
		err               error
	}{
		{"source error", "", "credential_unavailable", errors.New("secret store said " + testToken)},
		{"empty token", "", "invalid_credential", nil},
		{"header injection", testToken + "\r\nX-Secret: 1", "invalid_credential", nil},
		{"space", "secret token", "invalid_credential", nil},
		{"non-ASCII", "secret-tökén", "invalid_credential", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
			cfg := apiConfig(api)
			cfg.API.Credentials = func(context.Context) (string, error) { return tc.token, tc.err }
			_, _, err := Complete(context.Background(), cfg, userMessage, nil)
			failure := requireDiagnostic(t, err, EngineOpenAICompatible, PhasePreflight, tc.code)
			if failure.Kind != ErrorAuthentication || api.count() != 0 {
				t.Fatalf("kind %s, %d requests", failure.Kind, api.count())
			}
		})
	}
	t.Run("printing a config cannot print the token", func(t *testing.T) {
		cfg := apiConfig(nil)
		if printed := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg); strings.Contains(printed, testToken) {
			t.Fatal("configuration printed its credential")
		}
	})
}

func blockingAPI(started chan<- struct{}) *fakeAPI {
	return &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
		if started != nil {
			close(started)
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}}
}

func TestOpenAIChatCancellationAndTransportFailures(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		api := respondWith(200, `{}`)
		cfg := apiConfig(api)
		cfg.API.Credentials = func(context.Context) (string, error) { t.Fatal("credential resolved"); return "", nil }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := Complete(ctx, cfg, userMessage, nil); !errors.Is(err, context.Canceled) || api.count() != 0 {
			t.Fatalf("%v, %d requests", err, api.count())
		}
	})
	t.Run("cancelled in flight", func(t *testing.T) {
		started := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, usage, err := Complete(ctx, apiConfig(blockingAPI(started)), userMessage, nil)
		if !errors.Is(err, context.Canceled) || usage.Known {
			t.Fatalf("%v %#v", err, usage)
		}
		var failure *RequestError
		if errors.As(err, &failure) {
			t.Fatal("cancellation must stay cancellation, not a request failure")
		}
	})
	t.Run("timeout in flight", func(t *testing.T) {
		cfg := apiConfig(blockingAPI(nil))
		cfg.Timeout = 20 * time.Millisecond
		_, _, err := Complete(context.Background(), cfg, userMessage, nil)
		failure := requireAPIFailure(t, err, PhaseTransport, ErrorTimeout, "deadline_exceeded")
		if !errors.Is(err, context.DeadlineExceeded) || failure.Retryable() {
			t.Fatal("timeout lost deadline identity or became retryable")
		}
	})
	t.Run("caller deadline in flight", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, _, err := Complete(ctx, apiConfig(blockingAPI(nil)), userMessage, nil)
		requireAPIFailure(t, err, PhaseTransport, ErrorTimeout, "deadline_exceeded")
	})
	t.Run("credential source outlives Timeout", func(t *testing.T) {
		api := respondWith(200, `{}`)
		cfg := apiConfig(api)
		cfg.Timeout = 20 * time.Millisecond
		cfg.API.Credentials = func(ctx context.Context) (string, error) { <-ctx.Done(); return testToken, nil }
		_, _, err := Complete(context.Background(), cfg, userMessage, nil)
		requireAPIFailure(t, err, PhasePreflight, ErrorTimeout, "deadline_exceeded")
		if api.count() != 0 {
			t.Fatal("request sent after the credential deadline")
		}
	})
	t.Run("connection failure", func(t *testing.T) {
		api := &fakeAPI{respond: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: secret.invalid: " + testToken)
		}}
		_, _, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
		failure := requireAPIFailure(t, err, PhaseTransport, ErrorUnknown, "transport_failed")
		if failure.Retryable() || api.count() != 1 {
			t.Fatal("transport failure was retryable or retried")
		}
	})
	t.Run("body lost after status", func(t *testing.T) {
		api := &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
			body := io.MultiReader(strings.NewReader(`{"choices":[`), failingReader{})
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(body), Request: r}, nil
		}}
		_, _, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
		requireAPIFailure(t, err, PhaseResponse, ErrorUnknown, "transport_failed")
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("secret connection reset") }

func TestOpenAIDefaultTransportIgnoresAmbientProxy(t *testing.T) {
	transport, ok := apiTransport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("API transport must not route credentials through ambient proxy settings")
	}
}

func TestOpenAIModelDiscoveryIsNotYetOffered(t *testing.T) {
	_, err := discoverModels(context.Background(), apiConfig(nil), func(context.Context, Config, func(io.Reader, io.Writer) error) error {
		t.Fatal("discovery attempted for an API transport")
		return nil
	})
	if err == nil {
		t.Fatal("API discovery must be refused, not invented")
	}
}
