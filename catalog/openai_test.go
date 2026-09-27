package catalog

import (
	"context"
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

// fakeAPI stands in for every network round trip; no test reaches a network.
type fakeAPI struct {
	mu       sync.Mutex
	requests []*http.Request
	respond  func(*http.Request) (*http.Response, error)
}

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(context.Background()))
	f.mu.Unlock()
	return f.respond(r)
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
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

func apiProvider() harness.Provider {
	return harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{
		BaseURL:     "https://gateway.invalid/v1",
		Dialect:     harness.OpenAIChatCompletions,
		Credentials: func(context.Context) (string, error) { return testToken, nil },
	}}
}

func discoverAPI(t *testing.T, api *fakeAPI, p harness.Provider) ([]Model, error) {
	t.Helper()
	d := discoverer{timeout: defaultTimeout, transport: api, run: func(context.Context, invocation, exchange) error {
		t.Fatal("an API provider started a CLI")
		return nil
	}}
	return d.discover(context.Background(), p)
}

func TestOpenAIModelsRequestAndExtensions(t *testing.T) {
	api := respondWith(200, `{"object":"list","data":[
		{"id":"gpt-plain","object":"model","owned_by":"openai"},
		{"id":"xai/grok-4","name":"Grok 4","description":"Vercel entry","context_window":256000,"type":"language"},
		{"id":"anthropic/claude","name":"Claude","description":"OpenRouter entry","context_length":200000},
		{"id":"odd","name":{"en":"not a string"},"context_window":"128000","context_length":-1},
		{"id":""},
		{"id":"gpt-plain","name":"duplicate"}
	]}`, "Content-Type", "application/json; charset=utf-8")
	models, err := discoverAPI(t, api, apiProvider())
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "gpt-plain", Name: "gpt-plain"},
		{ID: "xai/grok-4", Name: "Grok 4", Description: "Vercel entry", ContextWindow: 256000},
		{ID: "anthropic/claude", Name: "Claude", Description: "OpenRouter entry", ContextWindow: 200000},
		{ID: "odd", Name: "odd"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("%+v", models)
	}
	if api.count() != 1 {
		t.Fatal("pagination or retry")
	}
	request := api.requests[0]
	if request.Method != http.MethodGet || request.URL.String() != "https://gateway.invalid/v1/models" || request.Body != nil && request.ContentLength != 0 {
		t.Fatalf("%s %s", request.Method, request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer "+testToken || request.Header.Get("Accept") != "application/json" {
		t.Fatal(request.Header)
	}
}

func TestOpenAILoopbackWithoutCredentialSendsNone(t *testing.T) {
	api := respondWith(200, `{"data":[{"id":"llama"}]}`)
	p := harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{BaseURL: "http://127.0.0.1:11434/v1", Dialect: harness.OpenAIChatCompletions, Unauthenticated: true}}
	models, err := discoverAPI(t, api, p)
	if err != nil || len(models) != 1 {
		t.Fatal(models, err)
	}
	if _, ok := api.requests[0].Header["Authorization"]; ok {
		t.Fatal("an unauthenticated request carried a credential")
	}
}

func TestOpenAIDiscoveryFailuresAreTyped(t *testing.T) {
	many := make([]string, maxModels+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"id":"m%d"}`, i)
	}
	for _, tc := range []struct {
		name   string
		api    *fakeAPI
		family harness.Family
		cause  harness.Cause
		code   string
	}{
		{"unauthorized", respondWith(401, `{"error":{"message":"secret invalid key `+testToken+`"}}`), harness.FailureRequest, harness.CauseAuthentication, "http_401"},
		{"not found", respondWith(404, `{"error":{"message":"secret"}}`), harness.FailureRequest, harness.CauseUnknown, "http_404"},
		{"redirect", respondWith(302, ``, "Location", "https://elsewhere.invalid/models"), harness.FailureRequest, harness.CauseUnknown, "redirect_refused"},
		{"html", respondWith(200, `<html>secret</html>`, "Content-Type", "text/html"), harness.FailureRequest, harness.CauseUnknown, "unexpected_media_type"},
		{"malformed", respondWith(200, `{"data":{"secret":true}}`), harness.FailureRequest, harness.CauseUnknown, "invalid_catalog"},
		{"no data", respondWith(200, `{"object":"list"}`), harness.FailureRequest, harness.CauseUnknown, "invalid_catalog"},
		{"trailing", respondWith(200, `{"data":[]} {"data":[]}`), harness.FailureRequest, harness.CauseUnknown, "invalid_catalog"},
		{"oversized", respondWith(200, `{"data":[],"pad":"`+strings.Repeat("x", apiBodyLimit)+`"}`), harness.FailureRequest, harness.CauseUnknown, "output_limit"},
		{"too many", respondWith(200, `{"data":[`+strings.Join(many, ",")+`]}`), harness.FailureRequest, harness.CauseUnknown, "catalog_limit"},
		{"echoed", respondWith(200, `{"data":[{"id":"`+testToken+`"}]}`), harness.FailureRequest, harness.CauseUnknown, "credential_echoed"},
		{"echoed escaped", respondWith(200, `{"data":[{"id":"m","description":"secret-dummy-token-not-real"}]}`), harness.FailureRequest, harness.CauseUnknown, "credential_echoed"},
		{"connection", &fakeAPI{respond: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: secret.invalid: " + testToken)
		}}, harness.FailureRequest, harness.CauseUnknown, "transport_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := discoverAPI(t, tc.api, apiProvider())
			failure := requireFailure(t, err, harness.OpenAICompatible, tc.family, tc.code)
			if failure.Cause != tc.cause {
				t.Fatalf("cause %s", failure.Cause)
			}
			if tc.api.count() != 1 {
				t.Fatal("retried or followed a redirect")
			}
		})
	}
}

func TestOpenAIRateLimitCarriesItsDelay(t *testing.T) {
	_, err := discoverAPI(t, respondWith(429, `{}`, "Retry-After", "7"), apiProvider())
	failure := requireFailure(t, err, harness.OpenAICompatible, harness.FailureRequest, "http_429")
	facts, _ := harness.ErrorFacts(err)
	if failure.Cause != harness.CauseRateLimited || !facts.Retryable || facts.RetryAfter != 7*time.Second {
		t.Fatalf("%+v", facts)
	}
}

// The credential source's own error is never retained: it may describe the
// caller's secret store, or contain the secret.
func TestOpenAICredentialFailuresNeverReachTheNetwork(t *testing.T) {
	for code, source := range map[string]harness.CredentialSource{
		"credential_unavailable": func(context.Context) (string, error) { return "", errors.New("secret store said " + testToken) },
		"invalid_credential":     func(context.Context) (string, error) { return "secret token\n", nil },
	} {
		api := respondWith(200, `{"data":[]}`)
		p := apiProvider()
		p.API.Credentials = source
		_, err := discoverAPI(t, api, p)
		failure := requireFailure(t, err, harness.OpenAICompatible, harness.FailurePreflight, code)
		if failure.Cause != harness.CauseAuthentication || api.count() != 0 {
			t.Fatalf("%+v sent=%d", failure, api.count())
		}
	}
	p := apiProvider()
	p.API.Credentials = nil
	_, err := discoverAPI(t, respondWith(200, `{}`), p)
	requireFailure(t, err, harness.OpenAICompatible, harness.FailurePreflight, "api_credentials_required")
}

func TestOpenAIDiscoveryPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
		cancel()
		return nil, r.Context().Err()
	}}
	d := discoverer{timeout: defaultTimeout, transport: api}
	if _, err := d.discover(ctx, apiProvider()); !errors.Is(err, context.Canceled) || errors.As(err, new(*Error)) {
		t.Fatalf("lost cancellation: %v", err)
	}
	blocked := &fakeAPI{respond: func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}}
	d = discoverer{timeout: 50 * time.Millisecond, transport: blocked}
	_, err := d.discover(context.Background(), apiProvider())
	requireFailure(t, err, harness.OpenAICompatible, harness.FailureRequest, "deadline_exceeded")
}
