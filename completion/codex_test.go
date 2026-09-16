package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCatalog = `{"models":[{"slug":"test-model","supported_reasoning_levels":[{"effort":"high"}],"shell_type":"unified_exec","apply_patch_tool_type":"freeform","experimental_supported_tools":["clock"],"tool_mode":"code_mode_only"}]}`

func TestCodexModelEffortAndIsolation(t *testing.T) {
	if got := catalogDefaultEffort([]byte(`{"models":[{"slug":"test-model","default_reasoning_level":"medium"}]}`), "test-model"); got != "medium" {
		t.Fatalf("catalog default effort=%q", got)
	}
	for _, tc := range []struct{ model, effort string }{{"missing", "high"}, {"test-model", "ultra"}} {
		if _, err := restrictedCatalog([]byte(testCatalog), tc.model, tc.effort); err == nil {
			t.Fatalf("accepted unsupported selection: %+v", tc)
		}
	}
	data, err := restrictedCatalog([]byte(testCatalog), "test-model", "high")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Models []map[string]any `json:"models"`
	}
	json.Unmarshal(data, &c)
	m := c.Models[0]
	if m["slug"] != "test-model" || m["shell_type"] != "disabled" || m["apply_patch_tool_type"] != nil || m["node_repl_disabled"] != true {
		t.Fatalf("unsafe catalog: %s", data)
	}
	t.Setenv("ASSISTANT_SECRET", "must-not-inherit")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	env, err := CodexEnvironment(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		if strings.Contains(e, "must-not-inherit") {
			t.Fatal("secret inherited")
		}
	}
	args := strings.Join(codexArgs(Config{Model: "test-model", Effort: "high"}, "/safe", "/models", "/schema", "/instructions"), " ")
	for _, want := range []string{"--ignore-user-config", "--ignore-rules", "--ephemeral", "--sandbox read-only", "project_doc_max_bytes=0", "features.hooks=false", "features.plugins=false", "features.shell_tool=false", "features.skip_host_skill_discovery=true", "model_reasoning_effort=\"high\""} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing boundary %s", want)
		}
	}
}

func TestCodexTransportProposesOnlyCallerTools(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	calls := 0
	reservations := 0
	cfg := Config{Engine: "codex", Model: "test-model", Effort: "high", CodexBin: "sh", BeforeRequest: func(context.Context) error { reservations++; return nil }}
	cfg.run = func(ctx context.Context, bin string, args []string, dir string, env []string, input string) ([]byte, error) {
		calls++
		if args[0] == "debug" {
			return []byte(testCatalog), nil
		}
		if probeURL := findProbeURL(args); probeURL != "" {
			response, err := http.Post(probeURL, "application/json", strings.NewReader(`{"model":"test-model","reasoning":{"effort":"high"}}`))
			if err != nil {
				return nil, err
			}
			response.Body.Close()
			return nil, errors.New("probe rejects deliberately")
		}
		if reservations != 1 {
			t.Fatal("inference ran before durable reservation")
		}
		if !strings.Contains(input, "only_this_tool") || strings.Contains(input, "create_project") {
			t.Fatal("transport injected unrelated tools into caller's catalog")
		}
		return []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"\",\"tool_calls\":[{\"name\":\"only_this_tool\",\"arguments\":\"{}\"}]}"}}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":5}}`), nil
	}
	tools := []Tool{{Type: "function", Function: Function{Name: "only_this_tool"}}}
	message, usage, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: "work"}}, tools)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || reservations != 1 || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "only_this_tool" || !usage.Known || usage.TotalTokens != 25 {
		t.Fatalf("calls=%d reservations=%d result=%+v usage=%+v", calls, reservations, message, usage)
	}
}

func postProbe(t *testing.T, url, body string) {
	t.Helper()
	response, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func findProbeURL(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "model_providers.harness_probe=") {
			start := strings.Index(arg, "base_url=\"") + len("base_url=\"")
			end := strings.Index(arg[start:], "\"")
			return arg[start:start+end] + "/responses"
		}
	}
	return ""
}

func TestCodexProbeFailsBeforeBillableCall(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, request := range []string{`{"model":"test-model","reasoning":{"effort":"high"},"tools":[{"name":"shell"}]}`, `{"model":"substitute","reasoning":{"effort":"high"}}`, `{"model":"test-model","reasoning":{"effort":"low"}}`} {
		t.Run(request, func(t *testing.T) {
			cfg := Config{Engine: "codex", Model: "test-model", Effort: "high", CodexBin: "sh", BeforeRequest: func(context.Context) error { t.Fatal("reserved a live call after unsafe probe"); return nil }}
			cfg.run = func(ctx context.Context, bin string, args []string, dir string, env []string, input string) ([]byte, error) {
				if args[0] == "debug" {
					return []byte(testCatalog), nil
				}
				url := findProbeURL(args)
				if url == "" {
					t.Fatal("live execution after unsafe probe")
				}
				res, err := http.Post(url, "application/json", strings.NewReader(request))
				if err == nil {
					res.Body.Close()
				}
				return nil, errors.New("rejected")
			}
			if _, _, err := Complete(context.Background(), cfg, nil, Tools()); err == nil {
				t.Fatal("unsafe probe accepted")
			}
		})
	}
}

func TestCodexRejectsUnsafeOrPartialOutput(t *testing.T) {
	for _, events := range []string{
		`{"type":"item.completed","item":{"type":"command_execution"}}`,
		`{"type":"turn.failed"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"\",\"tool_calls\":[{\"name\":\"shell\",\"arguments\":\"{}\"}]}"}}` + "\n" + `{"type":"turn.completed"}`,
	} {
		if _, _, err := parseCodex([]byte(events), Tools()); err == nil {
			t.Fatalf("accepted unsafe response %s", events)
		}
	}
}

// Opt-in integration test runs only a local rejecting provider with dummy auth.
// It neither calls a model nor uses the owner's login. New CLI builds must pass
// this same capability check before each real model invocation in production.
func TestInstalledCodexCapabilityProbe(t *testing.T) {
	bin := os.Getenv("AGENT_HARNESS_TEST_CODEX")
	if bin == "" {
		t.Skip("set AGENT_HARNESS_TEST_CODEX to test installed CLI without inference")
	}
	dir := t.TempDir()
	cfg := Config{Engine: "codex", CodexBin: bin, Model: "gpt-6-astra", Effort: "high", Timeout: 20 * time.Second}
	catalog, err := runCLI(context.Background(), cfg, bin, []string{"debug", "models", "--bundled"}, dir, codexCatalogEnvironment(dir), "")
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := restrictedCatalog(catalog, cfg.Model, cfg.Effort)
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := actionSchema(Tools())
	for name, data := range map[string][]byte{"models.json": restricted, "schema.json": schema, "instructions.txt": []byte(codexInstructions)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := codexArgs(cfg, dir, filepath.Join(dir, "models.json"), filepath.Join(dir, "schema.json"), filepath.Join(dir, "instructions.txt"))
	if err := probeCodex(context.Background(), cfg, bin, args, dir, codexCatalogEnvironment(dir)); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledCodexStructuredResponse(t *testing.T) {
	bin := os.Getenv("AGENT_HARNESS_TEST_CODEX")
	if bin == "" {
		t.Skip("set AGENT_HARNESS_TEST_CODEX for local-only protocol smoke")
	}
	dir := t.TempDir()
	cfg := Config{Engine: "codex", Model: "gpt-6-astra", Effort: "high", Timeout: 20 * time.Second}
	catalog, err := runCLI(context.Background(), cfg, bin, []string{"debug", "models", "--bundled"}, dir, codexCatalogEnvironment(dir), "")
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := restrictedCatalog(catalog, cfg.Model, cfg.Effort)
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := actionSchema(Tools())
	for name, data := range map[string][]byte{"models.json": restricted, "schema.json": schema, "instructions.txt": []byte(codexInstructions)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	envelope := `{"content":"Ready.","tool_calls":[]}`
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid Codex request")
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			t.Error("native tools exposed")
		}
		if body["model"] != cfg.Model {
			t.Error("model substituted")
		}

		text := body["text"].(map[string]any)
		format := text["format"].(map[string]any)
		if format["type"] != "json_schema" {
			t.Error("structured schema not sent")
		}
		if r.Header.Get("Authorization") != "Bearer fake-local-key" {
			t.Error("unexpected fake provider auth")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_test", "status": "in_progress"}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
			{"type": "response.output_text.delta", "item_id": "msg_test", "output_index": 0, "content_index": 0, "delta": envelope},
			{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": envelope, "annotations": []any{}}}}},
			{"type": "response.completed", "response": map[string]any{"id": "resp_test", "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 6, "total_tokens": 18, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer server.Close()
	args := codexArgs(cfg, dir, filepath.Join(dir, "models.json"), filepath.Join(dir, "schema.json"), filepath.Join(dir, "instructions.txt"))
	args = append(args, "-c", `model_provider="test_codex"`, "-c", `model_providers.test_codex={name="Local test",base_url="`+server.URL+`/v1",wire_api="responses",requires_openai_auth=true,request_max_retries=0,stream_max_retries=0}`)
	// A fake persisted API login exercises the same requires_openai_auth path.
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"OPENAI_API_KEY":"fake-local-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := runCLI(context.Background(), cfg, bin, args, dir, codexCatalogEnvironment(dir), `{"messages":[{"role":"user","content":"Hello"}],"available_tools":[]}`)
	if err != nil {
		t.Fatalf("Codex fake protocol: %v; %s", err, output)
	}
	result, usage, err := parseCodex(output, Tools())
	if err != nil {
		t.Fatalf("parse: %v; %s", err, output)
	}
	if result.Content != "Ready." || !usage.Known || usage.TotalTokens != 18 || requests.Load() != 1 {
		t.Fatalf("result=%+v usage=%+v requests=%d", result, usage, requests.Load())
	}
}

func TestCodexEnvelopeRequiresCompleteFields(t *testing.T) {
	for _, envelope := range []string{`{"tool_calls":[{"name":"read_state","arguments":"{}"}]}`, `{"content":null,"tool_calls":[{"name":"read_state","arguments":"{}"}]}`, `{"content":"answer"}`, `{"content":"answer","tool_calls":null}`} {
		item, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": envelope}})
		events := append(item, []byte("\n"+`{"type":"turn.completed"}`)...)
		if _, _, err := parseCodex(events, Tools()); err == nil {
			t.Fatalf("accepted malformed envelope %s", envelope)
		}
	}
}

func TestCodexProcessIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
	}{
		{"cancellation", "sleep 10 & wait", 80 * time.Millisecond},
		{"output limit", "while :; do printf '%01000d' 0; done", 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			_, err := runCLI(context.Background(), Config{Timeout: tc.timeout}, "/bin/sh", []string{"-c", tc.script}, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, "")
			if err == nil {
				t.Fatal("unbounded child succeeded")
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("child/process group was not promptly stopped")
			}
		})
	}
}

func TestCodexGlobalInstructionsFailBeforeProcessOrInference(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			if err := os.WriteFile(filepath.Join(home, name), []byte("unrelated owner coding instructions"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Engine: "codex", Model: "test-model", Effort: "high", run: func(context.Context, string, []string, string, []string, string) ([]byte, error) {
				t.Fatal("started subprocess with global instructions")
				return nil, nil
			}, BeforeRequest: func(context.Context) error { t.Fatal("reserved model with global instructions"); return nil }}
			if _, _, err := Complete(context.Background(), cfg, nil, Tools()); err == nil || !strings.Contains(err.Error(), "dedicated Codex home") {
				t.Fatalf("missing actionable isolation failure: %v", err)
			}
		})
	}
}

func environmentValue(env []string, key string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return ""
}
func TestConfiguredCodexHomeWinsOverAmbientHome(t *testing.T) {
	ambient, selected := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", ambient)
	t.Setenv("OPENAI_API_KEY", "provider-secret-not-for-codex")
	t.Setenv("SLACK_BOT_TOKEN", "integration-secret-not-for-codex")
	if err := os.WriteFile(filepath.Join(ambient, "AGENTS.md"), []byte("Ambient coding instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCodexHome(selected); err != nil {
		t.Fatal("validated ambient home instead of configured home", err)
	}
	if err := ValidateCodexHome(""); err == nil {
		t.Fatal("legacy fallback ignored ambient instruction boundary")
	}
	env, err := CodexEnvironment(selected)
	if err != nil {
		t.Fatal(err)
	}
	if environmentValue(env, "CODEX_HOME") != selected {
		t.Fatal("configured home lost", env)
	}
	for _, entry := range env {
		if strings.Contains(entry, "secret-not-for-codex") {
			t.Fatal("application credential inherited")
		}
	}
	if os.Getenv("CODEX_HOME") != ambient {
		t.Fatal("environment builder mutated daemon environment")
	}
	if _, err = CodexEnvironment("relative-home"); err == nil {
		t.Fatal("relative configured home accepted")
	}
}

func TestConfiguredCodexHomeGuardChecksSelectedDirectory(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, filename := range []string{"AGENTS.md", "AGENTS.override.md"} {
		t.Run(filename, func(t *testing.T) {
			selected := t.TempDir()
			if err := os.WriteFile(filepath.Join(selected, filename), []byte("Selected-home coding instructions"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Engine: "codex", CodexHome: selected, Model: "test-model", Effort: "high", run: func(context.Context, string, []string, string, []string, string) ([]byte, error) {
				t.Error("ran subprocess before checking configured home")
				return nil, errors.New("unexpected process")
			}, BeforeRequest: func(context.Context) error {
				t.Error("reserved inference before checking configured home")
				return errors.New("unexpected reservation")
			}}
			if _, _, err := Complete(context.Background(), cfg, nil, Tools()); err == nil {
				t.Fatal("selected global instructions were ignored")
			}
		})
	}
}

func TestConcurrentCodexRequestsKeepTheirSelectedHomes(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("CODEX_HOME", ambient)
	// A valid explicitly configured home must work even when the daemon inherited
	// a coding account whose global instructions prohibit transport use.
	if err := os.WriteFile(filepath.Join(ambient, "AGENTS.md"), []byte("Unrelated coding guidance"), 0600); err != nil {
		t.Fatal(err)
	}
	homes := []string{t.TempDir(), t.TempDir()}
	entered := make(chan struct{}, len(homes))
	release := make(chan struct{})
	results := make(chan error, len(homes))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, home := range homes {
		go func(selected string) {
			calls := 0
			cfg := Config{Engine: "codex", CodexHome: selected, CodexBin: "sh", Model: "test-model", Effort: "high"}
			cfg.run = func(ctx context.Context, _ string, args []string, dir string, env []string, _ string) ([]byte, error) {
				calls++
				if args[0] == "debug" {
					if environmentValue(env, "CODEX_HOME") != dir {
						return nil, errors.New("catalog did not use isolated environment")
					}
					return []byte(testCatalog), nil
				}
				if environmentValue(env, "CODEX_HOME") != selected {
					return nil, fmt.Errorf("request login home mismatch: got %q, want %q", environmentValue(env, "CODEX_HOME"), selected)
				}
				if environmentValue(env, "TMPDIR") != dir {
					return nil, errors.New("request temporary directory escaped its invocation")
				}
				if os.Getenv("CODEX_HOME") != ambient {
					return nil, errors.New("another request mutated the daemon environment")
				}
				if probeURL := findProbeURL(args); probeURL != "" {
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					request, err := http.NewRequestWithContext(ctx, "POST", probeURL, strings.NewReader(`{"model":"test-model","reasoning":{"effort":"high"},"tools":[]}`))
					if err != nil {
						return nil, err
					}
					response, err := http.DefaultClient.Do(request)
					if err != nil {
						return nil, err
					}
					response.Body.Close()
					return nil, errors.New("probe deliberately rejects")
				}
				return []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"Ready.\",\"tool_calls\":[]}"}}` + "\n" + `{"type":"turn.completed"}`), nil
			}
			result, _, err := Complete(ctx, cfg, []Message{{Role: "user", Content: "Hello"}}, Tools())
			if err == nil && (result.Content != "Ready." || calls != 3) {
				err = fmt.Errorf("unexpected transport result: calls=%d result=%+v", calls, result)
			}
			results <- err
		}(home)
	}
	for range homes {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("requests did not reach interleaved probes", ctx.Err())
		}
	}
	close(release)
	for range homes {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("CODEX_HOME") != ambient {
		t.Fatal("request changed ambient login home")
	}
}
