package completion

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The catalog transport spawns a real subprocess, so the transport tests
// re-exec this test binary as a synthetic CLI. No Codex or Claude install,
// login, account, credential or model is involved: the child only speaks the
// catalog handshake and reports the argv and home it was actually given.
//
// TestMain dispatches on the argv the transport builds rather than on an
// environment variable, because the transport replaces the child's environment
// entirely and no test variable survives into it. "app-server" and
// "--input-format" are the engine-specific invocations under test, and neither
// can appear in a `go test` command line.
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "app-server":
			os.Exit(runCatalogFixture("codex", os.Getenv("CODEX_HOME")))
		case "--input-format":
			os.Exit(runCatalogFixture("claude", os.Getenv("CLAUDE_CONFIG_DIR")))
		}
	}
	os.Exit(m.Run())
}

// catalogFixtureReport is what the synthetic CLI observed, written into the
// home directory the parent selected. Recording it there rather than in a
// shared temporary location is itself the assertion that the configured home
// reached the child.
type catalogFixtureReport struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

const catalogHangMarker = "hang"

func runCatalogFixture(engine, home string) int {
	if home == "" {
		return 3 // The configured home never reached the child.
	}
	// Only the selected login homes and the provider variables the tests seed
	// with synthetic values are recorded. Nothing else is read, so a regression
	// that widened the child's environment still cannot write a real credential
	// into this report or into a test failure message.
	env := map[string]string{}
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	report, err := json.Marshal(catalogFixtureReport{Args: os.Args[1:], Env: env})
	if err != nil {
		return 4
	}
	if os.WriteFile(filepath.Join(home, "invocation.json"), report, 0600) != nil {
		return 5
	}
	if _, err := os.Stat(filepath.Join(home, catalogHangMarker)); err == nil {
		// Announce readiness so the parent can synchronize on a live child, then
		// never answer again. The parent must unblock its own pipes on
		// cancellation and stop this child; process tests cover the kill itself.
		if _, err := os.Stdout.Write([]byte("\n")); err != nil {
			return 6
		}
		time.Sleep(30 * time.Second)
		return 7
	}
	return serveCatalog(engine)
}

func serveCatalog(engine string) int {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var message struct {
			ID        int    `json:"id"`
			Method    string `json:"method"`
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(in.Bytes(), &message) != nil {
			return 8
		}
		if engine == "claude" {
			if message.Type != "control_request" {
				continue
			}
			_ = out.Encode(map[string]any{"type": "control_response", "response": map[string]any{
				"request_id": message.RequestID,
				"subtype":    "success",
				"response": map[string]any{"models": []any{map[string]any{
					"value": "fixture-claude-model", "displayName": "Fixture Claude Model",
					"supportedEffortLevels": []string{"low"}, "defaultEffortLevel": "low",
				}}},
			}})
			return 0
		}
		switch message.Method {
		case "initialize":
			_ = out.Encode(map[string]any{"id": message.ID, "result": map[string]any{}})
		case "model/list":
			_ = out.Encode(map[string]any{"id": message.ID, "result": map[string]any{
				"data": []any{map[string]any{
					"model": "fixture-codex-model", "displayName": "Fixture Codex Model",
					"defaultReasoningEffort": "high", "isDefault": true,
					"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
				}},
				"nextCursor": "",
			}})
			return 0
		}
	}
	return 0
}
