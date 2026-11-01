package session

// Antigravity under the sandbox edge, checked against agy 1.4.12 by running
// it through a hostname-logging CONNECT proxy with a disposable home:
//
//   - Its Google-account login lives in the macOS Keychain ("Antigravity
//     CLI"), never in a file: a dummy ~/.gemini/oauth_creds.json is ignored.
//     The edge admits no keychain, so it cannot carry that login one way into
//     the runtime home. It runs agy's documented Gemini API-key mode instead:
//     modelProvider "gemini" in ~/.gemini/antigravity-cli/settings.json and
//     GEMINI_API_KEY, which the caller supplies as Edge.GeminiAPIKey.
//   - In that mode inference is POST generativelanguage.googleapis.com
//     /v1beta/models/<model>:streamGenerateContent?alt=sse, the public Gemini
//     API. It also reaches antigravity-unleash.goog (feature flags), an
//     auto-updater on *.run.app and storage.googleapis.com, even with
//     AGY_CLI_DISABLE_AUTO_UPDATE=1, and play.googleapis.com (telemetry).
//     None is admitted; agy completes a turn with all of them refused.
//   - Go's macOS certificate verifier asks trustd, which the edge does not
//     admit (OSStatus -26276). With SSL_CERT_FILE set agy verifies against
//     that bundle instead, so the edge names the system PEM bundle.
//   - It starts its language server on random localhost ports, so it needs
//     Sandbox.Loopback.
//   - An API-key login runs only in request-review, where a command needs a
//     permissions.allow rule: without one agy denies it and carries on.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	antigravitySettings  = ".gemini/antigravity-cli/settings.json"
	antigravityMCPConfig = ".gemini/config/mcp_config.json"
	antigravityProject   = ".gemini/config/projects/default-cli-project.json"
	antigravityProjectID = ".gemini/antigravity-cli/cache/default_project_id.txt"
	antigravityCABundle  = "/private/etc/ssl/cert.pem"
)

var antigravityEdge = &edgeEngine{
	hosts: []string{"generativelanguage.googleapis.com"},
	// Everything agy keeps under ~/.gemini is read-only to descendants:
	// settings, MCP servers, rules, agents, skills, hooks, plugins,
	// workflows, sidecars and the executables agy installs, whether or not
	// this list knows their names. Only the state a turn and a resume need
	// is writable, as observed under agy 1.4.12.
	protectRuntime: []string{".gemini"},
	writableRuntime: []string{
		".gemini/antigravity-cli/annotations/",
		".gemini/antigravity-cli/brain/",
		".gemini/antigravity-cli/conversations/",
		".gemini/antigravity-cli/crashes/",
		".gemini/antigravity-cli/implicit/",
		".gemini/antigravity-cli/log/",
		".gemini/antigravity-cli/presence/",
		".gemini/antigravity/",
		".gemini/antigravity-cli/conversation_summaries.db",
		".gemini/antigravity-cli/jetbox_summaries_proto.pb",
		".gemini/antigravity-cli/jetski_state.pbtxt",
		".gemini/antigravity-cli/installation_id",
		".gemini/antigravity-cli/last_check.timestamp",
		".gemini/antigravity-cli/cli.log",
		antigravityProjectID,
		antigravityProject,
		".gemini/antigravity-cli/cache/last_conversations.json",
		".gemini/antigravity-cli/cache/onboarding.json",
		".gemini/config/.migrated",
		// The shell integration agy writes before every command it runs, so a
		// planted copy never outlives the next command. The rest of bin/,
		// including the encoder it installs, stays read-only.
		".gemini/antigravity-cli/bin/agentapi",
	},
	standalone: true,
	readFiles:  []string{antigravityCABundle},
	// agy runs every command in a pseudo-terminal.
	terminals: true,
	owned:     []string{"GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "GOOGLE_APPLICATION_CREDENTIALS", "AGY_ADC_AUTH"},
	conflicts: func(o Options) error {
		if !o.Sandbox.Loopback {
			return refuse(o, "sandbox_edge", RefusedEdgeConflict, "Antigravity serves its language server on localhost: set Sandbox.Loopback")
		}
		if o.Sandbox.Web {
			return refuse(o, "sandbox_edge", RefusedEdgeConflict, "Antigravity web tools are unavailable under the edge")
		}
		return nil
	},
	credential: func(o Options) error {
		if o.Sandbox.Edge.GeminiAPIKey == "" {
			return refuse(o, "sandbox_edge", RefusedEdgeTokenRequired, "provide Edge.GeminiAPIKey: Antigravity's Google-account login is held in the macOS Keychain, which the edge does not admit")
		}
		if strings.ContainsAny(o.Sandbox.Edge.GeminiAPIKey, "\x00\r\n") {
			return refuse(o, "sandbox_edge", RefusedEdgeConflict, "invalid Gemini API key")
		}
		return nil
	},
	env: func(o Options) []string {
		return []string{"GEMINI_API_KEY=" + o.Sandbox.Edge.GeminiAPIKey, "SSL_CERT_FILE=" + antigravityCABundle}
	},
	baseURL: "GOOGLE_GEMINI_BASE_URL",
	prepare: func(_ context.Context, o Options, _ *launch) error {
		if err := writeAntigravitySettings(o.RuntimeHome, map[string]any{"modelProvider": "gemini"}); err != nil {
			return edgeFailure(o, CapabilityEdgeConfinementFailed)
		}
		return nil
	},
	canary: edgeCanary{
		setup: func(probe *Options) error {
			edge := *probe.Sandbox.Edge
			edge.GeminiAPIKey = "dummy-edge-gemini-key-not-a-login"
			probe.Sandbox.Edge = &edge
			// The canary's command alone is allowed, as an ACP canary answers
			// its one permission request allow_once.
			return writeAntigravitySettings(probe.RuntimeHome, map[string]any{"modelProvider": "gemini", "permissions": map[string]any{"allow": []string{"command(/usr/bin/perl)"}}})
		},
		auxiliary: func(w http.ResponseWriter, _ *http.Request, o Options, data []byte) bool {
			// A request offering no tools, such as the conversation's title.
			var body struct {
				Tools json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(data, &body) != nil || len(body.Tools) != 0 {
				return false
			}
			edgeGeminiAnswer(w, o, "Edge canary")
			return true
		},
		shell:  edgeGeminiShell,
		result: edgeGeminiResult,
		answer: edgeGeminiAnswer,
		drive: func(ctx context.Context, _ Options, w *streamWire) error {
			return w.send(ctx, map[string]any{"event": "user", "message": map[string]any{"content": "Local capability canary only."}})
		},
		// Loaders no protection names, which the read-only ~/.gemini covers.
		unlisted: []string{
			".gemini/antigravity-cli/plugins/edge-canary/hooks.json",
			".gemini/antigravity-cli/rules/edge-canary.md",
			".gemini/antigravity-cli/skills/edge-canary/SKILL.md",
			".gemini/antigravity-cli/bin/webm_encoder",
			".gemini/config/rules/edge-canary.md",
			".gemini/config/agents/edge-canary.md",
			".gemini/config/plugins/edge-canary/plugin.json",
			".gemini/skills/edge-canary/SKILL.md",
			".gemini/GEMINI.md",
			antigravitySettings,
			antigravityMCPConfig,
		},
		settled: func(m map[string]json.RawMessage) (bool, bool) {
			if str(m, "event") != "result" {
				return false, false
			}
			var r struct {
				Result struct {
					Status string `json:"status"`
				} `json:"result"`
			}
			_ = json.Unmarshal(mustMarshal(m), &r)
			return r.Result.Status == "SUCCESS", true
		},
	},
}

// writeAntigravitySettings replaces, in a runtime home, the files agy reads
// at start-up and cannot create inside its read-only ~/.gemini: its settings,
// its MCP server list, and its default project with the cache folder its id
// lives in. Each launch starts from these, so nothing an earlier session
// left in them is loaded. The runtime home is the session's own and may hold
// links it planted; written outside the sandbox, nothing here follows one.
func writeAntigravitySettings(runtimeHome string, settings map[string]any) error {
	// agy's own indented form: it rewrites any other spelling on load, and a
	// refused rewrite makes it fall back to its defaults.
	data, _ := json.MarshalIndent(settings, "", "  ")
	for path, content := range map[string][]byte{
		antigravitySettings:  append(data, '\n'),
		antigravityMCPConfig: nil,
		antigravityProject:   []byte("{\n  \"id\":  \"default-cli-project\",\n  \"name\":  \"CLI Project\",\n  \"projectResources\":  {}\n}"),
		antigravityProjectID: []byte("default-cli-project"),
	} {
		if err := writeBeneath(runtimeHome, path, content); err != nil {
			return err
		}
	}
	return nil
}

func edgeGeminiShell(w http.ResponseWriter, _ Options, data []byte, command string) bool {
	var body struct {
		Tools []struct {
			Functions []struct {
				Name string `json:"name"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if json.Unmarshal(data, &body) != nil {
		return false
	}
	found := false
	for _, tool := range body.Tools {
		for _, function := range tool.Functions {
			found = found || function.Name == "run_command"
		}
	}
	if !found {
		return false
	}
	call := map[string]any{"id": "call_edge", "name": "run_command", "args": map[string]any{"CommandLine": command, "toolSummary": "Edge canary", "WaitMsBeforeAsync": 10000}}
	writeGeminiChunk(w, map[string]any{"functionCall": call})
	return true
}

func edgeGeminiResult(data []byte) (string, bool) {
	var body struct {
		Contents []struct {
			Parts []struct {
				Response *struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Response struct {
						Output *string `json:"output"`
					} `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if json.Unmarshal(data, &body) != nil {
		return "", false
	}
	for _, content := range body.Contents {
		for _, part := range content.Parts {
			if r := part.Response; r != nil && r.Name == "run_command" && r.ID == "call_edge" {
				if r.Response.Output == nil {
					return "", true
				}
				return *r.Response.Output, true
			}
		}
	}
	return "", false
}

func edgeGeminiAnswer(w http.ResponseWriter, _ Options, text string) {
	writeGeminiChunk(w, map[string]any{"text": text})
}

// writeGeminiChunk streams one complete model turn as a single SSE chunk of
// the Gemini API's streamGenerateContent.
func writeGeminiChunk(w http.ResponseWriter, part map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": "STOP", "index": 0}}, "usageMetadata": map[string]int{"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2}}
	bytes, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", bytes)
}
