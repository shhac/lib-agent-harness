package completion

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func openRouterConfig(api *fakeAPI, routing harness.OpenRouterRouting) Config {
	cfg := apiConfig(api)
	cfg.Provider.API.BaseURL = "https://openrouter.invalid/api/v1"
	cfg.Provider.API.OpenRouter = &routing
	cfg.Model = "vendor/model:free"
	return cfg
}

// Routing is one typed field per documented key; nothing unset is sent.
func TestOpenRouterRoutingIsSentAsTypedFields(t *testing.T) {
	const prefix = `{"model":"vendor/model:free","stream":false,"messages":[{"role":"user","content":"secret prompt"}]`
	for _, tc := range []struct {
		name    string
		routing *harness.OpenRouterRouting
		want    string
	}{
		{"no option", nil, prefix + `}`},
		{"zero routing", &harness.OpenRouterRouting{}, prefix + `}`},
		{"require parameters", &harness.OpenRouterRouting{RequireParameters: true}, prefix + `,"provider":{"require_parameters":true}}`},
		{"allow collection", &harness.OpenRouterRouting{DataCollection: "allow"}, prefix + `,"provider":{"data_collection":"allow"}}`},
		{"deny collection", &harness.OpenRouterRouting{DataCollection: "deny"}, prefix + `,"provider":{"data_collection":"deny"}}`},
		{"both", &harness.OpenRouterRouting{RequireParameters: true, DataCollection: "deny"}, prefix + `,"provider":{"require_parameters":true,"data_collection":"deny"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
			cfg := openRouterConfig(api, harness.OpenRouterRouting{})
			cfg.Provider.API.OpenRouter = tc.routing
			if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
				t.Fatal(err)
			}
			sameJSON(t, api.last(t).body, tc.want)
		})
	}
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	_, err := Complete(context.Background(), openRouterConfig(api, harness.OpenRouterRouting{DataCollection: "maybe"}), userMessage, nil)
	requireAPIFailure(t, err, PhasePreflight, harness.CauseUnknown, "api_openrouter_data_collection_invalid")
	if api.count() != 0 {
		t.Fatal("an invalid routing option reached the network")
	}
}

// A free model's rate limit is an ordinary 429: typed, retryable, carrying
// the delay only when the response gives a usable one. Nothing retries.
func TestOpenRouterFreeModelRateLimit(t *testing.T) {
	const body = `{"error":{"code":429,"message":"secret rate limit","metadata":{"raw":"secret"}}}`
	for _, tc := range []struct {
		name   string
		header []string
		want   time.Duration
	}{
		{"delay given", []string{"Retry-After", "60"}, 60 * time.Second},
		{"no delay", nil, 0},
		{"date delay", []string{"Retry-After", "Wed, 30 Sep 2026 07:28:00 GMT"}, 0},
		{"delay over an hour", []string{"Retry-After", "86400"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(429, body, tc.header...)
			_, err := Complete(context.Background(), openRouterConfig(api, harness.OpenRouterRouting{}), userMessage, nil)
			failure := requireAPIFailure(t, err, PhaseResponse, harness.CauseRateLimited, "http_429")
			if !failure.Retryable() || failure.RetryAfter != tc.want {
				t.Fatalf("%+v", failure)
			}
			if api.count() != 1 {
				t.Fatalf("made %d requests; the caller owns retries", api.count())
			}
		})
	}
}

// An error inside a 200 response reads the same whether it came whole or
// streamed after some output. Under API.OpenRouter its integer code is a
// status; without the option, or for any other code, it is provider_error.
func TestOpenRouterEmbeddedFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		error     string
		option    bool
		cause     harness.Cause
		code      string
		retryable bool
	}{
		{"rate limited", `{"code":429,"message":"secret"}`, true, harness.CauseRateLimited, "http_429", true},
		{"insufficient credits", `{"code":402,"message":"secret"}`, true, harness.CauseQuotaExhausted, "insufficient_credits", false},
		{"upstream failed", `{"code":502,"message":"secret","metadata":{"provider_name":"secret"}}`, true, harness.CauseUnknown, "http_502", false},
		{"no provider", `{"code":503,"message":"secret"}`, true, harness.CauseUnavailable, "http_503", true},
		{"rate limited without the option", `{"code":429,"message":"secret"}`, false, harness.CauseUnknown, "provider_error", false},
		{"credits without the option", `{"code":402,"message":"secret"}`, false, harness.CauseUnknown, "provider_error", false},
		{"unavailable without the option", `{"code":503,"message":"secret"}`, false, harness.CauseUnknown, "provider_error", false},
		{"string code wins", `{"code":429,"type":"insufficient_quota","message":"secret"}`, true, harness.CauseQuotaExhausted, "insufficient_quota", false},
		{"float code", `{"code":429.0,"message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
		{"exponent code", `{"code":4.29e2,"message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
		{"string digits", `{"code":"429","message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
		{"success code", `{"code":200,"message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
		{"out of range", `{"code":999,"message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
		{"negative", `{"code":-429,"message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
		{"no code", `{"message":"secret"}`, true, harness.CauseUnknown, "provider_error", false},
	} {
		paths := map[string]func() *fakeAPI{
			// OpenRouter's whole response: an error object and no choices.
			"whole": func() *fakeAPI {
				return respondWith(200, `{"id":"gen-1","error":`+tc.error+`,`+testUsage+`}`)
			},
			// OpenRouter's streamed failure after output: the error beside a
			// choice finishing with "error", then the usage chunk.
			"streamed": func() *fakeAPI {
				return streamWith(
					event(`{"id":"gen-1","choices":[{"index":0,"delta":{"role":"assistant","content":"secret partial"}}]}`),
					event(`{"id":"gen-1","error":`+tc.error+`,"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`),
					event(`{"id":"gen-1","choices":[],`+testUsage+`}`),
					streamDone,
				)
			},
		}
		for path, api := range paths {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				cfg := openRouterConfig(api(), harness.OpenRouterRouting{})
				if !tc.option {
					cfg.Provider.API.OpenRouter = nil
				}
				cfg.Provider.API.Streaming = path == "streamed"
				result, err := Complete(context.Background(), cfg, userMessage, nil)
				failure := requireAPIFailure(t, err, PhaseResponse, tc.cause, tc.code)
				if failure.Retryable() != tc.retryable || failure.RetryAfter != 0 {
					t.Fatalf("%+v", failure)
				}
				if !reflect.DeepEqual(result.Message, Message{}) {
					t.Fatalf("output before the error was returned: %#v", result.Message)
				}
				if !result.Usage.Known || result.Usage.Total() != 7 {
					t.Fatalf("usage beside the error was lost: %+v", result.Usage)
				}
				facts, ok := harness.ErrorFacts(err)
				if !ok || facts.Cause != tc.cause || facts.RetryAfter != 0 {
					t.Fatalf("facts %+v", facts)
				}
			})
		}
	}
}

// A 200 error that reflects the credential is discarded before it is
// classified, streamed or not: the echo outranks the rate limit it reports.
func TestOpenRouterEmbeddedFailureEchoingTheCredential(t *testing.T) {
	failure := `{"code":429,"message":"Bearer ` + testToken + `"}`
	for path, api := range map[string]*fakeAPI{
		"whole":    respondWith(200, `{"error":`+failure+`,`+testUsage+`}`),
		"streamed": streamWith(event(`{"error":`+failure+`,"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}`), event(`{"choices":[],`+testUsage+`}`), streamDone),
	} {
		t.Run(path, func(t *testing.T) {
			cfg := openRouterConfig(api, harness.OpenRouterRouting{})
			cfg.Provider.API.Streaming = path == "streamed"
			result, err := Complete(context.Background(), cfg, userMessage, nil)
			if failure := requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "credential_echoed"); failure.Retryable() {
				t.Fatalf("%+v", failure)
			}
			if !reflect.DeepEqual(result.Message, Message{}) || !result.Usage.Known {
				t.Fatalf("%+v", result)
			}
		})
	}
}
