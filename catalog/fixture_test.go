package catalog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The catalog transport spawns a real subprocess, so the transport tests
// re-exec this test binary as a synthetic CLI. No Codex, Claude or Grok
// install, login, account, credential or model is involved: the child only
// speaks the catalog handshake and reports the argv and environment it was
// actually given.
//
// TestMain dispatches on the argv the transport builds rather than on an
// environment variable, because the transport replaces the child's environment
// entirely and no test variable survives into it. "app-server",
// "--input-format" and "--no-leader" are the engine-specific invocations under
// test, and none can appear in a `go test` command line.
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "app-server":
			os.Exit(runCatalogFixture("codex", os.Getenv("CODEX_HOME")))
		case "--input-format":
			os.Exit(runCatalogFixture("claude", os.Getenv("CLAUDE_CONFIG_DIR")))
		case "--no-leader":
			os.Exit(runCatalogFixture("grok", os.Getenv("GROK_HOME")))
		}
	}
	os.Exit(m.Run())
}

// fixtureReport is what the synthetic CLI observed, written into the home
// directory the parent selected. Recording it there rather than in a shared
// temporary location is itself the assertion that the configured home reached
// the child.
type fixtureReport struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

const hangMarker = "hang"

// recordedKeys are the only variables the fixture reads: the login homes,
// the provider keys the tests seed with synthetic values, and two of Grok's
// reduced-telemetry overrides. A regression that widened the child's
// environment still cannot write a real credential into a report.
var recordedKeys = []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "GROK_HOME", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "GROK_TELEMETRY_ENABLED", "GROK_CLAUDE_MCPS_ENABLED"}

func runCatalogFixture(engine, home string) int {
	if home == "" {
		return 3 // The configured home never reached the child.
	}
	env := map[string]string{}
	for _, key := range recordedKeys {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	report, err := json.Marshal(fixtureReport{Args: os.Args[1:], Env: env})
	if err != nil {
		return 4
	}
	if os.WriteFile(filepath.Join(home, "invocation.json"), report, 0600) != nil {
		return 5
	}
	if _, err := os.Stat(filepath.Join(home, hangMarker)); err == nil {
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
		switch engine {
		case "claude":
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
		case "grok":
			if message.Method != "initialize" {
				return 9 // Anything but initialize (session/new above all) is refused.
			}
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","method":"_x.ai/mcp/servers_updated","params":{}}` + "\n")
			_, _ = os.Stdout.WriteString(grokFixture + "\n")
			// Stay up, as the real agent does: the parent must stop the process
			// rather than wait for it.
			time.Sleep(30 * time.Second)
			return 10
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

// grokFixture is grok 1.0.41's initialize reply, trimmed of capabilities and
// auth methods but keeping the host and MCP metadata that must not leak.
const grokFixture = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[{"id":"secret-auth-method"}],"_meta":{"agentVersion":"1.0.41","hostname":"secret-host","mcpServers":[{"name":"secret-mcp"}],"modelState":{"currentModelId":"grok-4.7","availableModels":[{"modelId":"grok-4.7","name":"Grok 4.7","description":"SpaceXAI's latest frontier model","_meta":{"totalContextTokens":500000,"agentType":"grok-build-plan","supportsReasoningEffort":true,"reasoningEffort":"high","reasoningEfforts":[{"id":"xhigh","value":"xhigh","label":"Extra High","description":"Maximum reasoning for the hardest tasks.","default":false},{"id":"high","value":"high","label":"High","description":"Thorough reasoning and quality. Recommended.","default":true},{"id":"medium","value":"medium","label":"Medium","description":"Strong quality with a faster turnaround.","default":false},{"id":"low","value":"low","label":"Low","description":"Fastest responses. Best for simple tasks.","default":false}]}}]}}}}`
