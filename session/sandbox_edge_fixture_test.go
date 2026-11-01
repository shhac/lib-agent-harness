//go:build !windows

package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// A native protocol fixture for lifecycle and secret tests. It makes no model
// request. Real provider inference and native shell execution are tested by
// the separately selected installed-CLI canary, never by this fixture's echoes.
func fakeEdgeSession(args []string) int {
	claude := len(args) > 0 && args[0] == "-p"
	for i, arg := range args {
		if arg == "--settings" && i+1 < len(args) {
			var doc struct {
				Sandbox struct {
					Enabled bool `json:"enabled"`
				} `json:"sandbox"`
			}
			if json.Unmarshal([]byte(args[i+1]), &doc) != nil || doc.Sandbox.Enabled {
				return 2
			}
		}
	}
	if os.Getenv("HTTPS_PROXY") == "" || os.Getenv("HOME") == "" {
		return 2
	}
	if len(args) > 0 && args[0] == "-p=" {
		return fakeEdgeAntigravity(args)
	}
	if claude && (os.Getenv("CLAUDE_CONFIG_DIR") == "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "") {
		return 2
	}
	data, _ := json.Marshal(map[string]string{"proxy": os.Getenv("HTTPS_PROXY"), "home": os.Getenv("HOME"), "config": os.Getenv("CLAUDE_CONFIG_DIR"), "token": os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")})
	if os.WriteFile(os.Getenv("HOME")+"/edge-fixture-env.json", data, 0600) != nil {
		return 2
	}
	fmt.Fprintln(os.Stderr, os.Getenv("HTTPS_PROXY"), os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"))
	input := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for input.Scan() {
		var frame map[string]json.RawMessage
		if json.Unmarshal(input.Bytes(), &frame) != nil {
			return 2
		}
		if claude {
			if str(frame, "type") == "control_request" {
				if out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{}}}) != nil {
					return 2
				}
				continue
			}
			if str(frame, "type") == "user" {
				if strings.Contains(string(input.Bytes()), "edge-fixture-wait") {
					continue
				}
				if strings.Contains(string(input.Bytes()), "edge-fixture-crash") {
					return 3
				}
				out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": os.Getenv("HTTPS_PROXY") + " " + os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")}}}})
				out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": os.Getenv("HTTPS_PROXY") + " " + os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")})
				continue
			}
			continue
		}
		id := frame["id"]
		switch str(frame, "method") {
		case "initialized":
			continue
		case "initialize":
			out.Encode(map[string]any{"id": id, "result": map[string]any{}})
		case "thread/start", "thread/resume":
			var p map[string]any
			_ = json.Unmarshal(frame["params"], &p)
			if p["sandbox"] != "danger-full-access" || p["approvalPolicy"] != "never" {
				return 2
			}
			out.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "edge-fixture-thread"}, "sandbox": map[string]any{"type": "dangerFullAccess"}}})
		case "turn/start":
			if strings.Contains(string(input.Bytes()), "edge-fixture-crash") {
				return 3
			}
			out.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "edge-fixture-turn"}}})
			if strings.Contains(string(input.Bytes()), "edge-fixture-refresh") {
				// Simulate the CLI observing an expired access token and attempting
				// refresh. The fake provider must never receive this rotating token.
				data, err := os.ReadFile(os.Getenv("CODEX_HOME") + "/auth.json")
				if err != nil || !strings.Contains(string(data), `"expires_at":1`) {
					return 2
				}
				proxy, _ := url.Parse(os.Getenv("HTTPS_PROXY"))
				client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
				response, _ := client.Post("https://auth.openai.com/oauth/token", "application/json", strings.NewReader(`{"refresh_token":"dummy-rotating-token"}`))
				if response != nil {
					response.Body.Close()
				}
				continue
			}
			if strings.Contains(string(input.Bytes()), "edge-fixture-wait") {
				continue
			}
			out.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "edge-fixture-thread", "turnId": "edge-fixture-turn", "item": map[string]any{"type": "agentMessage", "id": "edge-message", "text": os.Getenv("HTTPS_PROXY")}}})
			out.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "edge-fixture-thread", "turn": map[string]any{"id": "edge-fixture-turn", "status": "completed"}}})
		default:
			out.Encode(map[string]any{"id": id, "result": map[string]any{}})
		}
	}
	return 0
}

// fakeEdgeAntigravity is agy's stream-json under the edge: it requires the
// API-key login the library prepares and never its own sandbox flag.
func fakeEdgeAntigravity(args []string) int {
	conversation := "edge-fixture-conversation"
	for _, arg := range args {
		if arg == "--sandbox" || strings.HasPrefix(arg, "--dangerously") {
			return 2
		}
		if id, ok := strings.CutPrefix(arg, "--conversation="); ok {
			conversation = id
		}
	}
	settings, err := os.ReadFile(os.Getenv("HOME") + "/.gemini/antigravity-cli/settings.json")
	var doc struct {
		ModelProvider string          `json:"modelProvider"`
		Permissions   json.RawMessage `json:"permissions"`
	}
	if err != nil || json.Unmarshal(settings, &doc) != nil || doc.ModelProvider != "gemini" || doc.Permissions != nil {
		return 2
	}
	if os.Getenv("GEMINI_API_KEY") == "" || os.Getenv("SSL_CERT_FILE") != antigravityCABundle {
		return 2
	}
	data, _ := json.Marshal(map[string]string{"proxy": os.Getenv("HTTPS_PROXY"), "home": os.Getenv("HOME"), "token": os.Getenv("GEMINI_API_KEY")})
	if os.WriteFile(os.Getenv("HOME")+"/edge-fixture-env.json", data, 0600) != nil {
		return 2
	}
	fmt.Fprintln(os.Stderr, os.Getenv("HTTPS_PROXY"), os.Getenv("GEMINI_API_KEY"))
	out := json.NewEncoder(os.Stdout)
	out.Encode(map[string]any{"event": "init", "conversation_id": conversation, "init": map[string]any{"model": "", "permission_mode": AntigravityRequestReview, "tools": []string{"run_command"}}})
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		if strings.Contains(input.Text(), "edge-fixture-crash") {
			return 3
		}
		leak := os.Getenv("HTTPS_PROXY") + " " + os.Getenv("GEMINI_API_KEY")
		out.Encode(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": conversation, "step_index": 1, "state": "DONE", "step_type": "agent_response", "text_delta": leak, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
		out.Encode(map[string]any{"event": "result", "result": map[string]any{"conversation_id": conversation, "status": "SUCCESS", "response": leak, "num_turns": 1, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}
	return 0
}
