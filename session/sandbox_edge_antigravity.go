package session

// Antigravity under the sandbox edge, checked against agy 1.4.2 by running
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
	"os"
	"path/filepath"
	"strings"
)

const (
	antigravitySettings  = ".gemini/antigravity-cli/settings.json"
	antigravityMCPConfig = ".gemini/config/mcp_config.json"
	antigravityCABundle  = "/private/etc/ssl/cert.pem"
)

var antigravityEdge = &edgeEngine{
	hosts: []string{"generativelanguage.googleapis.com"},
	// agy's own and its global configuration, rules, skills, hooks, plugins,
	// workflows and sidecars: what a later launch would load.
	protectRuntime: []string{
		antigravitySettings,
		".gemini/antigravity-cli/hooks.json",
		".gemini/antigravity-cli/builtin",
		".gemini/GEMINI.md",
		".gemini/AGENTS.md",
		".gemini/config/config.json",
		".gemini/config/hooks.json",
		antigravityMCPConfig,
		".gemini/config/plugins",
		".gemini/config/skills",
		".gemini/config/sidecars",
		".gemini/config/workflows",
		".gemini/config/workflows.json",
		".gemini/config/global_workflows",
		".gemini/jetski",
	},
	readFiles: []string{antigravityCABundle},
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

// writeAntigravitySettings replaces agy's settings and its MCP server list in
// a runtime home, so state an earlier launch left there is never loaded. The
// edge protects both from descendants, so only the library writes them.
func writeAntigravitySettings(runtimeHome string, settings map[string]any) error {
	// agy's own indented form: it rewrites any other spelling on load, and a
	// refused rewrite makes it fall back to its defaults.
	data, _ := json.MarshalIndent(settings, "", "  ")
	// agy creates an empty server list when there is none; the edge refuses
	// it that write.
	for path, content := range map[string][]byte{antigravitySettings: append(data, '\n'), antigravityMCPConfig: nil} {
		path = filepath.Join(runtimeHome, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
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
