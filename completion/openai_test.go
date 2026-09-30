package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
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
		Provider: harness.Provider{
			Engine: harness.OpenAICompatible,
			API: harness.API{
				BaseURL:     "https://gateway.invalid/v1",
				Dialect:     harness.OpenAIChatCompletions,
				Credentials: func(context.Context) (string, error) { return testToken, nil },
			},
		},
		Model:     "provider/test-model",
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

func requireAPIFailure(t *testing.T, err error, phase ErrorPhase, kind harness.Cause, code string) *RequestError {
	t.Helper()
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Engine != harness.OpenAICompatible || failure.Phase != phase || failure.Cause != kind || failure.Code != code {
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

func TestOpenAIChatHTTPFailuresAreClassifiedWithoutProviderText(t *testing.T) {
	errorBody := func(code, kind string) string {
		return `{"error":{"message":"secret provider text ` + testToken + `","type":"` + kind + `","code":"` + code + `"}}`
	}
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		header     []string
		kind       harness.Cause
		code       string
		retryAfter time.Duration
	}{
		{"unauthorized", 401, errorBody("invalid_api_key", "invalid_request_error"), nil, harness.CauseAuthentication, "http_401", 0},
		{"forbidden", 403, errorBody("", "secret"), nil, harness.CausePermissionDenied, "http_403", 0},
		{"model not found", 404, errorBody("model_not_found", "invalid_request_error"), nil, harness.CauseModelUnavailable, "model_not_found", 0},
		{"wrong path", 404, `secret not found`, nil, harness.CauseUnknown, "http_404", 0},
		{"context length", 400, errorBody("context_length_exceeded", "invalid_request_error"), nil, harness.CauseContextLimit, "context_length_exceeded", 0},
		{"bad request", 400, errorBody("secret_code", "invalid_request_error"), nil, harness.CauseUnknown, "http_400", 0},
		{"too large", 413, ``, nil, harness.CauseContextLimit, "http_413", 0},
		{"rate limited", 429, errorBody("rate_limit_exceeded", "requests"), []string{"Retry-After", "7"}, harness.CauseRateLimited, "http_429", 7 * time.Second},
		{"rate limited unreadable body", 429, `secret`, nil, harness.CauseRateLimited, "http_429", 0},
		{"rate limited date delay", 429, ``, []string{"Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT"}, harness.CauseRateLimited, "http_429", 0},
		{"rate limited excessive delay", 429, ``, []string{"Retry-After", "7200"}, harness.CauseRateLimited, "http_429", 0},
		{"quota exhausted", 429, errorBody("insufficient_quota", "insufficient_quota"), []string{"Retry-After", "7"}, harness.CauseQuotaExhausted, "insufficient_quota", 0},
		{"quota exhausted by type", 429, errorBody("", "insufficient_quota"), nil, harness.CauseQuotaExhausted, "insufficient_quota", 0},
		{"unavailable", 503, ``, []string{"Retry-After", "3"}, harness.CauseUnavailable, "http_503", 3 * time.Second},
		{"overloaded", 529, errorBody("", "overloaded_error"), nil, harness.CauseOverloaded, "http_529", 0},
		{"server error", 500, errorBody("", "server_error"), []string{"Retry-After", "3"}, harness.CauseUnknown, "http_500", 0},
		{"bad gateway", 502, ``, nil, harness.CauseUnknown, "http_502", 0},
		{"gateway timeout", 504, ``, nil, harness.CauseUnknown, "http_504", 0},
		{"unexpected success status", 201, chatBody(chatChoice(`"stop"`, `{"content":"secret"}`)), nil, harness.CauseUnknown, "http_201", 0},
		{"redirect", 307, ``, []string{"Location", "https://elsewhere.invalid/v1/chat/completions"}, harness.CauseUnknown, "redirect_refused", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(tc.status, tc.body, tc.header...)
			reply, usage, err := messageAndUsage(Complete(context.Background(), apiConfig(api), userMessage, nil))
			failure := requireAPIFailure(t, err, PhaseResponse, tc.kind, tc.code)
			if failure.RetryAfter != tc.retryAfter {
				t.Fatalf("retry after %v", failure.RetryAfter)
			}
			wantRetry := tc.kind == harness.CauseRateLimited || tc.kind == harness.CauseUnavailable || tc.kind == harness.CauseOverloaded
			if failure.Retryable() != wantRetry {
				t.Fatalf("retryable %v", failure.Retryable())
			}
			if !reflect.DeepEqual(reply, Message{}) || usage != (harness.Usage{}) {
				t.Fatalf("failed status produced %#v %#v", reply, usage)
			}
			if api.count() != 1 {
				t.Fatalf("made %d requests; redirects and retries are never followed", api.count())
			}
		})
	}
	var failure *RequestError
	_, err := Complete(context.Background(), apiConfig(respondWith(404, errorBody("model_not_found", ""))), userMessage, nil)
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
		_, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
		requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "unexpected_media_type")
	}
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"`+strings.Repeat("x", apiResponseLimit)+`"}`)), "Content-Type", "application/json; charset=utf-8")
	_, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
	requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "output_limit")
}

func TestOpenAIChatDiscardsAResponseEchoingTheCredential(t *testing.T) {
	escaped := `s` + strings.TrimPrefix(testToken, "s")
	for name, body := range map[string]string{
		"raw":            chatBody(chatChoice(`"stop"`, `{"content":"Bearer `+testToken+`"}`)),
		"escaped text":   chatBody(chatChoice(`"stop"`, `{"content":"`+escaped+`"}`)),
		"tool arguments": chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("call_1", "lookup", `{"k":"`+testToken+`"}`)))),
	} {
		t.Run(name, func(t *testing.T) {
			reply, usage, err := messageAndUsage(Complete(context.Background(), apiConfig(respondWith(200, body)), userMessage, lookupTool))
			requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "credential_echoed")
			if !reflect.DeepEqual(reply, Message{}) || !usage.Known {
				t.Fatalf("reply %#v usage %#v", reply, usage)
			}
		})
	}
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
		cfg.Provider.API.Credentials = func(context.Context) (string, error) { t.Fatal("credential resolved"); return "", nil }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Complete(ctx, cfg, userMessage, nil); !errors.Is(err, context.Canceled) || api.count() != 0 {
			t.Fatalf("%v, %d requests", err, api.count())
		}
	})
	t.Run("cancelled in flight", func(t *testing.T) {
		started := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, usage, err := messageAndUsage(Complete(ctx, apiConfig(blockingAPI(started)), userMessage, nil))
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
		_, err := Complete(context.Background(), cfg, userMessage, nil)
		failure := requireAPIFailure(t, err, PhaseTransport, harness.CauseTimeout, "deadline_exceeded")
		if !errors.Is(err, context.DeadlineExceeded) || failure.Retryable() {
			t.Fatal("timeout lost deadline identity or became retryable")
		}
	})
	t.Run("caller deadline in flight", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := Complete(ctx, apiConfig(blockingAPI(nil)), userMessage, nil)
		requireAPIFailure(t, err, PhaseTransport, harness.CauseTimeout, "deadline_exceeded")
	})
	t.Run("credential source outlives Timeout", func(t *testing.T) {
		api := respondWith(200, `{}`)
		cfg := apiConfig(api)
		cfg.Timeout = 20 * time.Millisecond
		cfg.Provider.API.Credentials = func(ctx context.Context) (string, error) { <-ctx.Done(); return testToken, nil }
		_, err := Complete(context.Background(), cfg, userMessage, nil)
		requireAPIFailure(t, err, PhasePreflight, harness.CauseTimeout, "deadline_exceeded")
		if api.count() != 0 {
			t.Fatal("request sent after the credential deadline")
		}
	})
	t.Run("connection failure", func(t *testing.T) {
		api := &fakeAPI{respond: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: secret.invalid: " + testToken)
		}}
		_, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
		failure := requireAPIFailure(t, err, PhaseTransport, harness.CauseUnknown, "transport_failed")
		if failure.Retryable() || api.count() != 1 {
			t.Fatal("transport failure was retryable or retried")
		}
	})
	t.Run("body lost after status", func(t *testing.T) {
		api := &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
			body := io.MultiReader(strings.NewReader(`{"choices":[`), failingReader{})
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(body), Request: r}, nil
		}}
		_, err := Complete(context.Background(), apiConfig(api), userMessage, nil)
		requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "transport_failed")
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("secret connection reset") }
