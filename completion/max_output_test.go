package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// An engine with no proven mechanism refuses a cap before any process,
// probe, reservation or request, rather than running uncapped.
func TestMaxOutputTokensRefusedWithoutAProvenMechanism(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Grok} {
		t.Run(string(engine), func(t *testing.T) {
			if engine == harness.Grok && runtime.GOOS == "windows" {
				t.Skip("Grok completion is refused as an engine on Windows")
			}
			cfg := Config{Provider: cliProvider(engine, "fixture", ""), Model: "test-model", WorkDirRoot: t.TempDir(), MaxOutputTokens: 1000}
			cfg.BeforeRequest = func(context.Context) error { t.Fatal("refused cap reached BeforeRequest"); return nil }
			cfg.run = func(context.Context, string, []string, string, []string, string) ([]byte, error) {
				t.Fatal("refused cap started a process")
				return nil, nil
			}
			_, err := Complete(context.Background(), cfg, userMessage, Tools())
			failure := requireDiagnostic(t, err, engine, PhasePreflight, "max_output_tokens_unsupported")
			if facts := failure.HarnessFacts(); facts.Family != harness.FailureCapability {
				t.Fatalf("family %s, want capability", facts.Family)
			}
		})
	}
}

func TestMaxOutputTokensNegativeIsRefused(t *testing.T) {
	for _, engine := range append([]harness.Engine{harness.Grok}, cliEngines...) {
		cfg := Config{Provider: cliProvider(engine, "fixture", ""), Model: "test-model", MaxOutputTokens: -1}
		cfg.run = func(context.Context, string, []string, string, []string, string) ([]byte, error) {
			t.Fatal("negative cap started a process")
			return nil, nil
		}
		_, err := Complete(context.Background(), cfg, userMessage, nil)
		if engine == harness.Grok && runtime.GOOS == "windows" {
			continue
		}
		requireDiagnostic(t, err, engine, PhasePreflight, "invalid_limits")
	}
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
	cfg := apiConfig(api)
	cfg.MaxOutputTokens = -1
	_, err := Complete(context.Background(), cfg, userMessage, nil)
	requireDiagnostic(t, err, harness.OpenAICompatible, PhasePreflight, "invalid_limits")
	if api.count() != 0 {
		t.Fatal("negative cap reached the transport")
	}
}

// The cap travels as max_completion_tokens, and nothing is sent without one.
func TestOpenAIChatMaxCompletionTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int
		want string
	}{
		{"capped", 256, "256"},
		{"uncapped", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"content":"ok"}`)))
			cfg := apiConfig(api)
			cfg.MaxOutputTokens = tc.cap
			if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(api.last(t).body, &body); err != nil {
				t.Fatal(err)
			}
			if string(body["max_completion_tokens"]) != tc.want || body["max_tokens"] != nil {
				t.Fatalf("max_completion_tokens %s, max_tokens %s", body["max_completion_tokens"], body["max_tokens"])
			}
		})
	}
	api := respondWith(200, chatBody(chatChoice(`"length"`, `{"content":"cut"}`)))
	cfg := apiConfig(api)
	cfg.MaxOutputTokens = 8
	result, err := Complete(context.Background(), cfg, userMessage, nil)
	requireAPIFailure(t, err, PhaseResponse, harness.CauseOutputTruncated, "output_truncated")
	if result.Message.Content != "" || len(result.Message.ToolCalls) != 0 {
		t.Fatal("a reply cut off at the cap was returned")
	}
}

// claudeProbeRequest is what the local provider receives from a Claude CLI
// that honoured the restricted flags, with max_tokens as given (nil omits it).
func claudeProbeRequest(args []string, maxTokens *int) []byte {
	var schema json.RawMessage
	effort := ""
	for i, arg := range args {
		if arg == "--json-schema" {
			schema = json.RawMessage(args[i+1])
		}
		if arg == "--effort" {
			effort = args[i+1]
		}
	}
	request := map[string]any{"tools": []any{map[string]any{"name": "StructuredOutput", "input_schema": schema}}, "system": []any{map[string]string{"type": "text", "text": codexInstructions}}, "output_config": map[string]string{"effort": effort}}
	if maxTokens != nil {
		request["max_tokens"] = *maxTokens
	}
	data, _ := json.Marshal(request)
	return data
}

func envEntry(env []string, key string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value, true
		}
	}
	return "", false
}

func TestClaudeMaxOutputTokens(t *testing.T) {
	testenv.RequireLoopback(t) // Constrained CLI preflight starts a local refusal provider.
	sent := func(n int) *int { return &n }
	for _, tc := range []struct {
		name       string
		cap        int
		ambient    string
		probeMax   *int
		wantVar    string
		wantCode   string
		wantResult bool
	}{
		{name: "cap reaches the request", cap: 512, probeMax: sent(512), wantVar: "512", wantResult: true},
		{name: "lowered to the model limit", cap: 1000000, probeMax: sent(64000), wantVar: "1000000", wantResult: true},
		{name: "uncapped ignores an ambient cap", ambient: "7", probeMax: sent(32000), wantResult: true},
		{name: "cap exceeded", cap: 512, probeMax: sent(32000), wantVar: "512", wantCode: "probe_changed_max_output_tokens"},
		{name: "cap missing", cap: 512, wantVar: "512", wantCode: "probe_changed_max_output_tokens"},
		{name: "cap zeroed", cap: 512, probeMax: sent(0), wantVar: "512", wantCode: "probe_changed_max_output_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ambient != "" {
				t.Setenv(claudeMaxOutputVariable, tc.ambient)
			}
			root := t.TempDir()
			reserved, inference := false, 0
			cfg := Config{Provider: cliProvider(harness.Claude, "test-claude", filepath.Join(root, "login")), Model: "test-model", Effort: "high", WorkDirRoot: root, Timeout: time.Second, MaxOutputTokens: tc.cap}
			cfg.BeforeRequest = func(context.Context) error { reserved = true; return nil }
			cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
				value, set := envEntry(env, claudeMaxOutputVariable)
				if value != tc.wantVar || set != (tc.wantVar != "") {
					t.Fatalf("%s=%q (set %v), want %q", claudeMaxOutputVariable, value, set, tc.wantVar)
				}
				if base, probe := envEntry(env, "ANTHROPIC_BASE_URL"); probe {
					response, err := http.Post(base+"/v1/messages", "application/json", bytes.NewReader(claudeProbeRequest(args, tc.probeMax)))
					if err == nil {
						response.Body.Close()
					}
					return nil, err
				}
				if !reserved {
					t.Fatal("inference before BeforeRequest")
				}
				inference++
				return []byte(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"content":"Ready","tool_calls":[]}}`), nil
			}
			result, err := Complete(context.Background(), cfg, userMessage, Tools())
			if tc.wantCode != "" {
				requireDiagnostic(t, err, harness.Claude, PhasePreflight, tc.wantCode)
				if reserved || inference != 0 {
					t.Fatal("billable work after an unproven cap")
				}
				return
			}
			if err != nil || result.Message.Content != "Ready" || inference != 1 {
				t.Fatalf("result %+v err %v inference %d", result.Message, err, inference)
			}
		})
	}
}
