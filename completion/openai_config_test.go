package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

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
		cfg.Provider.API.BaseURL = base
		if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
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

// A local model server that takes no credential: no Authorization header, and
// an answer that happens to contain any text is not mistaken for an echo.
func TestOpenAIChatUnauthenticatedLoopback(t *testing.T) {
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	cfg := apiConfig(api)
	cfg.Provider.API.BaseURL, cfg.Provider.API.Credentials, cfg.Provider.API.Unauthenticated = "http://localhost:11434/v1", nil, true
	result, err := Complete(context.Background(), cfg, userMessage, nil)
	if err != nil || result.Message.Content != "ok" {
		t.Fatalf("reply %#v err %v", result.Message, err)
	}
	if header := api.last(t).header.Get("Authorization"); header != "" {
		t.Fatalf("unauthenticated request sent %q", header)
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
		{"dialect required", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.Dialect = "" }, "api_dialect_required"},
		{"responses dialect not yet", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.Dialect = "openai-responses" }, "api_dialect_unsupported"},
		{"empty URL", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.BaseURL = "" }, "api_base_url_invalid"},
		{"relative URL", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.BaseURL = "/secret/v1" }, "api_base_url_invalid"},
		{"other scheme", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.BaseURL = "ftp://gateway.invalid/v1" }, "api_base_url_invalid"},
		{"user information", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Provider.API.BaseURL = "https://user:secret@gateway.invalid/v1"
		}, "api_base_url_invalid"},
		{"query", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Provider.API.BaseURL = "https://gateway.invalid/v1?key=secret"
		}, "api_base_url_invalid"},
		{"fragment", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.BaseURL = "https://gateway.invalid/v1#secret" }, "api_base_url_invalid"},
		{"plaintext remote", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.BaseURL = "http://gateway.invalid/v1" }, "api_base_url_insecure"},
		{"plaintext private network", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.BaseURL = "http://10.0.0.1/v1" }, "api_base_url_insecure"},
		{"credentials required", func(c *Config, _ *[]Message, _ *[]Tool) { c.Provider.API.Credentials = nil }, "api_credentials_required"},
		{"effort without a parameter", func(c *Config, _ *[]Message, _ *[]Tool) { c.Effort = "low" }, "api_effort_parameter_required"},
		{"unknown effort parameter", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Effort, c.Provider.API.EffortParameter = "low", "secret.effort"
		}, "api_effort_parameter_unsupported"},
		{"unknown effort parameter without an effort", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Provider.API.EffortParameter = "secret.effort"
		}, "api_effort_parameter_unsupported"},
		{"overlong effort", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Effort, c.Provider.API.EffortParameter = strings.Repeat("x", 33), harness.EffortReasoningEffort
		}, "api_effort_invalid"},
		{"malformed effort", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Effort, c.Provider.API.EffortParameter = `high","model":"other`, harness.EffortReasoningEffort
		}, "api_effort_invalid"},
		{"unauthenticated remote", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Provider.API.Credentials, c.Provider.API.Unauthenticated = nil, true
		}, "api_unauthenticated_remote"},
		{"unauthenticated beside credentials", func(c *Config, _ *[]Message, _ *[]Tool) {
			c.Provider.API.BaseURL, c.Provider.API.Unauthenticated = "http://127.0.0.1:11434/v1", true
		}, "api_credentials_conflict"},
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
		// Arguments are always an object; an endpoint would refuse the request.
		{"non-object parameters", func(_ *Config, _ *[]Message, tools *[]Tool) {
			*tools = []Tool{{Type: "function", Function: Function{Name: "lookup", Parameters: map[string]any{"type": "array"}}}}
		}, "invalid_tool_catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
			cfg := apiConfig(api)
			asked := false
			if cfg.Provider.API.Credentials != nil {
				cfg.Provider.API.Credentials = func(context.Context) (string, error) { asked = true; return testToken, nil }
			}
			cfg.BeforeRequest = func(context.Context) error { t.Fatal("refused request reached BeforeRequest"); return nil }
			messages, tools := userMessage, []Tool(nil)
			tc.mutate(&cfg, &messages, &tools)
			_, err := Complete(context.Background(), cfg, messages, tools)
			requireDiagnostic(t, err, harness.OpenAICompatible, PhasePreflight, tc.code)
			if asked || api.count() != 0 {
				t.Fatal("refused request resolved a credential or reached the transport")
			}
		})
	}
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	cfg := apiConfig(api)
	cfg.MaxContextBytes = 1024
	_, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: strings.Repeat("secret", 200)}}, nil)
	requireAPIFailure(t, err, PhasePreflight, harness.CauseContextLimit, "context_bytes")
	if api.count() != 0 {
		t.Fatal("oversized request was sent")
	}
}

func TestOpenAIChatCredentialSource(t *testing.T) {
	t.Run("resolved once, after BeforeRequest, under the request deadline", func(t *testing.T) {
		var order []string
		cfg := apiConfig(respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`))))
		cfg.BeforeRequest = func(context.Context) error { order = append(order, "before"); return nil }
		cfg.Provider.API.Credentials = func(ctx context.Context) (string, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("credential source is not bounded by Timeout")
			}
			order = append(order, "credential")
			return testToken, nil
		}
		if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
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
		cfg.Provider.API.Credentials = func(context.Context) (string, error) { t.Fatal("credential resolved"); return "", nil }
		if _, err := Complete(context.Background(), cfg, userMessage, nil); !errors.Is(err, gate) || api.count() != 0 {
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
			cfg.Provider.API.Credentials = func(context.Context) (string, error) { return tc.token, tc.err }
			_, err := Complete(context.Background(), cfg, userMessage, nil)
			failure := requireDiagnostic(t, err, harness.OpenAICompatible, PhasePreflight, tc.code)
			if failure.Cause != harness.CauseAuthentication || api.count() != 0 {
				t.Fatalf("kind %s, %d requests", failure.Cause, api.count())
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
