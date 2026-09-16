package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeNativeLoginEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "secret")
	t.Setenv("CLAUDE_CONFIG_DIR", "/wrong")
	t.Setenv("USER", "test-owner")
	selectedHome := filepath.Join(t.TempDir(), "login")
	env, err := ClaudeEnvironment(selectedHome)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains("\n"+joined+"\n", "\nUSER=test-owner\n") {
		t.Fatal("native keychain login identity was dropped")
	}
	if !strings.Contains(joined, "CLAUDE_CONFIG_DIR="+selectedHome) || strings.Contains(joined, "secret") || strings.Contains(joined, "/wrong") {
		t.Fatal("environment leaked ambient auth/overrides")
	}
	if _, err := ClaudeEnvironment("relative"); err == nil {
		t.Fatal("accepted relative home")
	}
}

func TestClaudeTransportIsCoordinationOnlyAndUsesState(t *testing.T) {
	root := t.TempDir()
	calls := 0
	reserved := false
	cfg := Config{Engine: "claude", ClaudeHome: filepath.Join(root, "login"), ClaudeBin: "test-claude", Model: "test-model", Effort: "high", WorkDirRoot: root, MaxContextBytes: 100000, Timeout: time.Second, BeforeRequest: func(context.Context) error { reserved = true; return nil }}
	cfg.run = func(_ context.Context, _ string, args []string, dir string, env []string, input string) ([]byte, error) {
		calls++
		for _, entry := range env {
			if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
				if reserved {
					t.Fatal("reserved capacity for local probe")
				}
				return nil, answerClaudeProbe(args, strings.TrimPrefix(entry, "ANTHROPIC_BASE_URL="))
			}
		}
		if !reserved {
			t.Fatal("missing budget reservation")
		}
		for _, want := range []string{"--safe-mode", "--tools=", "--setting-sources=", "--strict-mcp-config", "--mcp-config={\"mcpServers\":{}}", "--settings={\"disableAllHooks\":true}", "--no-session-persistence"} {
			found := false
			for _, arg := range args {
				if arg == want {
					found = true
				}
			}
			if !found {
				t.Fatal("missing isolation flag", want)
			}
		}
		if filepath.Dir(dir) != filepath.Join(root, "model-runs") {
			t.Fatal("scratch not under state", dir)
		}
		if !json.Valid([]byte(input)) || !strings.Contains(input, "available_tools") {
			t.Fatal("invalid action input")
		}
		return []byte(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"content":"Ready","tool_calls":[]},"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":4,"cache_creation_input_tokens":3}}`), nil
	}
	result, usage, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: "Hello"}}, Tools())
	if err != nil || result.Content != "Ready" || !usage.Known || usage.TotalTokens != 19 || calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	entries, err := os.ReadDir(filepath.Join(root, "model-runs"))
	if err != nil || len(entries) != 0 {
		t.Fatal("scratch not cleaned", err)
	}
}

func TestClaudeRejectsNativeToolsAndMalformedResult(t *testing.T) {
	for _, input := range []string{
		`{"type":"system","subtype":"init","tools":["Bash"]}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"result","subtype":"success","structured_output":{"content":"wrong"}}`,
		`{"type":"result","subtype":"success","structured_output":{"content":"wrong","tool_calls":[{"name":"Bash","arguments":"{}"}]}}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"error":"secret"}`,
	} {
		_, _, err := parseClaude([]byte(input), nil)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe Claude result accepted or error leaked")
		}
	}
}

func TestClaudeDoesNotRetryFailure(t *testing.T) {
	calls := 0
	cfg := Config{Engine: "claude", ClaudeBin: "test", Model: "test", MaxContextBytes: 10000, run: func(context.Context, string, []string, string, []string, string) ([]byte, error) {
		calls++
		return nil, errors.New("secret")
	}}
	_, _, err := claudeComplete(context.Background(), cfg, []Message{{Role: "user", Content: "test"}}, nil)
	if calls != 1 || err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(calls, err)
	}
}

func answerClaudeProbe(args []string, base string) error {
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
	data, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"name": "StructuredOutput", "input_schema": schema}}, "system": []any{map[string]string{"type": "text", "text": codexInstructions}}, "output_config": map[string]string{"effort": effort}})
	response, err := http.Post(base+"/v1/messages", "application/json", bytes.NewReader(data))
	if err == nil {
		response.Body.Close()
	}
	return err
}

func TestClaudeCapabilityProbeRejectsNativeToolsAndInstructions(t *testing.T) {
	schema := []byte(`{"type":"object"}`)
	for _, fixture := range []string{
		`{"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`,
		`{"tools":[{"name":"StructuredOutput","input_schema":{"type":"object"}}],"system":[{"type":"text","text":"unwanted global instructions"}]}`,
	} {
		if validClaudeProbe([]byte(fixture), schema, "") {
			t.Fatal("unsafe probe accepted")
		}
	}
}

func TestClaudeDefaultHomeRetainsNativeKeychainNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/ambient-override")
	env, err := ClaudeEnvironment(filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") {
			t.Fatal("native default should use native auth lookup")
		}
	}
}
