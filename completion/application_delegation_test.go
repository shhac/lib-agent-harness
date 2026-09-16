package completion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

// A transport without native agents must still carry the caller's delegation
// function. The CLI proposes an action; only the application may execute it.
func TestApplicationDelegationAcrossConstrainedTransports(t *testing.T) {
	// Use an existing executable for preflight on every platform. The injected
	// transport handles every invocation; no CLI or native login is required.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "claude"} {
		t.Run(name, func(t *testing.T) {
			tools := []Tool{{Type: "function", Function: Function{Name: "delegate", Description: "Commission an approved application worker"}}}
			cfg := Config{Engine: name, Model: "test-model", Effort: "high", CodexBin: binary, ClaudeBin: binary, CodexHome: t.TempDir(), ClaudeHome: t.TempDir(), WorkDirRoot: t.TempDir()}
			cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, input string) ([]byte, error) {
				if args[0] == "debug" {
					return []byte(testCatalog), nil
				}
				if url := findProbeURL(args); url != "" {
					response, err := http.Post(url, "application/json", strings.NewReader(`{"model":"test-model","reasoning":{"effort":"high"},"tools":[]}`))
					if err != nil {
						return nil, err
					}
					response.Body.Close()
					return nil, errors.New("intentional local probe rejection")
				}
				for _, entry := range env {
					if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
						return nil, answerClaudeProbe(args, strings.TrimPrefix(entry, "ANTHROPIC_BASE_URL="))
					}
				}
				var payload struct {
					Tools []Tool `json:"available_tools"`
				}
				if err := json.Unmarshal([]byte(input), &payload); err != nil || len(payload.Tools) != 1 || payload.Tools[0].Function.Name != "delegate" {
					t.Fatal("application catalog changed", err)
				}
				if !strings.Contains(codexInstructions, "native-tool restriction does not prohibit proposing the supplied application functions") {
					t.Fatal("transport conflates native and application authority")
				}
				action := map[string]any{"content": "", "tool_calls": []any{map[string]string{"name": "delegate", "arguments": `{"task":"bounded work"}`}}}
				if name == "claude" {
					raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "structured_output": action})
					return raw, nil
				}
				raw, _ := json.Marshal(action)
				event, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": string(raw)}})
				return append(event, []byte("\n{\"type\":\"turn.completed\"}\n")...), nil
			}
			result, _, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: "Proceed with the approved work"}}, tools)
			if err != nil || len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Name != "delegate" {
				t.Fatalf("delegation proposal lost: %+v, %v", result, err)
			}
		})
	}
}
