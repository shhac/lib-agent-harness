package catalog

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

// supported_parameters is read per entry: a value that is not a bounded array
// of strings leaves that entry's parameters unknown and the rest of the
// catalog as it was.
func TestOpenAIModelsRecordSupportedParameters(t *testing.T) {
	many := make([]string, maxParameters+1)
	for i := range many {
		many[i] = fmt.Sprintf(`"p%d"`, i)
	}
	for _, tc := range []struct {
		name  string
		value string
		want  []string
		known bool
		tools bool
	}{
		{"with tools", `"supported_parameters":["temperature","tools","tool_choice"]`, []string{"temperature", "tools", "tool_choice"}, true, true},
		{"without tools", `"supported_parameters":["temperature","max_tokens"]`, []string{"temperature", "max_tokens"}, true, false},
		{"empty", `"supported_parameters":[]`, []string{}, true, false},
		{"null", `"supported_parameters":null`, nil, false, false},
		{"missing", `"other":1`, nil, false, false},
		{"string", `"supported_parameters":"tools"`, nil, false, false},
		{"number inside", `"supported_parameters":["tools",7]`, nil, false, false},
		{"too many", `"supported_parameters":[` + strings.Join(many, ",") + `]`, nil, false, false},
		{"most allowed", `"supported_parameters":[` + strings.Join(many[:maxParameters], ",") + `]`, nil, true, false},
		{"entry too long", `"supported_parameters":["tools","` + strings.Repeat("x", maxParameterBytes+1) + `"]`, nil, false, false},
		{"longest entry", `"supported_parameters":["` + strings.Repeat("x", maxParameterBytes) + `"]`, []string{strings.Repeat("x", maxParameterBytes)}, true, false},
		{"empty string", `"supported_parameters":["tools",""]`, nil, false, false},
		{"duplicates", `"supported_parameters":["tools","seed","tools","seed"]`, []string{"tools", "seed"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(200, `{"data":[
				{"id":"before","supported_parameters":["tools"]},
				{"id":"subject","context_length":8192,`+tc.value+`},
				{"id":"after","name":"After"}
			]}`)
			models, err := discoverAPI(t, api, apiProvider())
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 3 || models[0].ID != "before" || !models[0].ParametersKnown || models[2].Name != "After" || models[2].ParametersKnown {
				t.Fatalf("the rest of the catalog changed: %+v", models)
			}
			subject := models[1]
			if subject.ID != "subject" || subject.ContextWindow != 8192 {
				t.Fatalf("%+v", subject)
			}
			if subject.ParametersKnown != tc.known || tc.want != nil && !reflect.DeepEqual(subject.Parameters, tc.want) {
				t.Fatalf("parameters %#v known %v", subject.Parameters, subject.ParametersKnown)
			}
			if !tc.known && subject.Parameters != nil {
				t.Fatalf("unknown parameters kept values: %#v", subject.Parameters)
			}
			if tc.name == "most allowed" && len(subject.Parameters) != maxParameters {
				t.Fatalf("%d parameters", len(subject.Parameters))
			}
			if supported, known := SupportsTools(subject); supported != tc.tools || known != tc.known {
				t.Fatalf("SupportsTools = %v, %v", supported, known)
			}
		})
	}
}

func TestSupportsTools(t *testing.T) {
	for _, tc := range []struct {
		name             string
		model            Model
		supported, known bool
	}{
		{"known with tools", Model{ID: "m", Parameters: []string{"seed", "tools"}, ParametersKnown: true}, true, true},
		{"known without", Model{ID: "m", Parameters: []string{"tool_choice", "seed"}, ParametersKnown: true}, false, true},
		{"known empty", Model{ID: "m", Parameters: []string{}, ParametersKnown: true}, false, true},
		{"unknown", Model{ID: "m"}, false, false},
		// Parameters without ParametersKnown say nothing.
		{"unknown with values", Model{ID: "m", Parameters: []string{"tools"}}, false, false},
	} {
		if supported, known := SupportsTools(tc.model); supported != tc.supported || known != tc.known {
			t.Errorf("%s: %v, %v", tc.name, supported, known)
		}
	}
}

// Callers cache catalogs. Parameters survive a round trip, and an entry cached
// before they were recorded decodes as unknown, never as "no tools".
func TestModelParametersRoundTripAndOldCachesAreUnknown(t *testing.T) {
	for _, model := range []Model{
		{ID: "m", Name: "m", Parameters: []string{"tools"}, ParametersKnown: true},
		{ID: "m", Name: "m", Parameters: []string{}, ParametersKnown: true},
		{ID: "m", Name: "m"},
	} {
		encoded, err := json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Model
		if err = json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(decoded, model) {
			t.Fatalf("%s decoded as %+v (%v)", encoded, decoded, err)
		}
	}
	var cached Model
	if err := json.Unmarshal([]byte(`{"id":"m","name":"m","efforts":null,"efforts_known":false,"is_default":true}`), &cached); err != nil {
		t.Fatal(err)
	}
	if _, known := SupportsTools(cached); known {
		t.Fatalf("an old cached entry claimed parameters: %+v", cached)
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
		// The escape hides the token from the raw body, so only the decoded
		// parameter shows it.
		{"echoed parameter", respondWith(200, `{"data":[{"id":"m","supported_parameters":["tools","secret-dummy-token-not-real"]}]}`), harness.FailureRequest, harness.CauseUnknown, "credential_echoed"},
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
