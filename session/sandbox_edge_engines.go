package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// edgeEngine is what the sandbox edge needs from one engine: where its
// provider lives, how it runs with its own sandbox off, the credential it
// carries, the state it is given after proof, and how the canary drives one
// native shell call through a fake provider. An engine without one is not
// offered the edge.
type edgeEngine struct {
	// hosts are the provider hosts the proxy admits on 443.
	hosts []string
	// refreshHost is the login-refresh host refused even when a caller lists
	// it or names it as BaseURL; an authenticated attempt to reach it fails
	// the session with edge_login_refresh_blocked.
	refreshHost string
	// preferences are the macOS preference domains the CLI must read.
	preferences []string
	// protectRuntime and protectWork are the engine's own configuration and
	// instruction loaders, relative to RuntimeHome and WorkDir, that
	// descendants cannot write; readFiles are public single files it reads.
	protectRuntime, protectWork, readFiles []string
	// terminals admits pseudo-terminals the sandbox itself creates.
	terminals bool
	// owned are variables the engine's edge sets itself, which a caller may
	// not add, beyond the proxy, trust and home variables every edge owns.
	owned []string
	// conflicts refuses options the engine cannot run with under the edge;
	// credential refuses a missing or malformed caller-supplied login. Each
	// returns nil when the options are acceptable.
	conflicts, credential func(Options) error
	// args switch the CLI's own sandbox off; the edge contains it instead.
	args func(Options) []string
	// env are the engine's own production variables, and baseURL the
	// variable that carries Edge.BaseURL.
	env     func(Options) []string
	baseURL string
	// prepare readies the runtime home after a complete proof.
	prepare func(context.Context, Options, *launch) error
	// hostTools gives the session the open tool channel; nil refuses
	// Sandbox.Tools under the edge.
	hostTools func(*toolHost) ([]string, error)
	canary    edgeCanary
}

// edgeCanary is how the pre-launch canary drives the installed CLI to one
// native shell call through a fake provider speaking the engine's wire.
type edgeCanary struct {
	// setup writes the disposable runtime's login and configuration, and may
	// adjust the probe's options for the canary alone.
	setup func(probe *Options) error
	// auxiliary answers a provider request that is neither the turn nor its
	// shell call, such as a token count or a title, and reports whether it did.
	auxiliary func(w http.ResponseWriter, r *http.Request, o Options, data []byte) bool
	// shell answers the turn's request with the scripted shell call, or
	// reports that the request offered no native shell tool.
	shell func(w http.ResponseWriter, o Options, data []byte, command string) bool
	// result is the output of the scripted call, read only from its own
	// tool result.
	result func(data []byte) (string, bool)
	// answer ends the turn with the nonce as the model's text.
	answer func(w http.ResponseWriter, o Options, nonce string)
	// drive starts the canary's one turn on a live wire.
	drive func(ctx context.Context, o Options, w *streamWire) error
	// settled reads whether a frame ends the turn, and whether successfully.
	settled func(m map[string]json.RawMessage) (success, done bool)
}

// edgeOf is the engine's edge entry, or nil when the edge is not offered.
func edgeOf(o Options) *edgeEngine { return engines[o.Provider.Engine].edge }

var codexEdge = &edgeEngine{
	hosts:       []string{"api.openai.com", "chatgpt.com"},
	refreshHost: "auth.openai.com",
	preferences: []string{"com.openai.codex"},
	args: func(o Options) []string {
		args := codexSandboxArgs(*o.Sandbox)
		// Remove the permission-profile selection: the enclosing OS boundary owns
		// reads, writes and egress. Legacy thread sandbox is deliberately explicit.
		var out []string
		for i := 0; i < len(args); i += 2 {
			if strings.HasPrefix(args[i+1], "default_permissions=") || strings.HasPrefix(args[i+1], "permissions.") {
				continue
			}
			out = append(out, args[i:i+2]...)
		}
		return out
	},
	baseURL:   "OPENAI_BASE_URL",
	prepare:   prepareCodexSandbox,
	hostTools: codexSandbox.hostTools,
	canary: edgeCanary{
		shell:  edgeCodexShell,
		result: edgeCodexResult,
		answer: edgeCodexAnswer,
		drive:  edgeCodexDrive,
		settled: func(m map[string]json.RawMessage) (bool, bool) {
			if string(m["method"]) != `"turn/completed"` {
				return false, false
			}
			var v struct {
				Turn struct {
					Status string `json:"status"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(m["params"], &v)
			return v.Turn.Status == "completed", true
		},
	},
}

var claudeEdge = &edgeEngine{
	hosts: []string{"api.anthropic.com"},
	conflicts: func(o Options) error {
		if o.Sandbox.Web {
			return refuse(o, "sandbox_edge", RefusedEdgeConflict, "Claude WebFetch and WebSearch are unavailable under the edge")
		}
		return nil
	},
	credential: func(o Options) error {
		if o.Sandbox.Edge.ClaudeToken == "" {
			return refuse(o, "sandbox_edge", RefusedEdgeTokenRequired, "provide Edge.ClaudeToken from claude setup-token")
		}
		return nil
	},
	args: claudeSandboxArgs,
	env: func(o Options) []string {
		return []string{"CLAUDE_CONFIG_DIR=" + filepath.Join(o.RuntimeHome, "claude"), "CLAUDE_CODE_TMPDIR=" + filepath.Join(o.RuntimeHome, "tmp"), "CLAUDE_CODE_OAUTH_TOKEN=" + o.Sandbox.Edge.ClaudeToken, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	},
	baseURL: "ANTHROPIC_BASE_URL",
	prepare: func(_ context.Context, o Options, _ *launch) error {
		if err := os.MkdirAll(filepath.Join(o.RuntimeHome, "claude"), 0700); err != nil {
			return edgeFailure(o, CapabilityEdgeConfinementFailed)
		}
		return nil
	},
	hostTools: claudeSandbox.hostTools,
	canary: edgeCanary{
		auxiliary: func(w http.ResponseWriter, r *http.Request, _ Options, _ []byte) bool {
			if !strings.HasSuffix(r.URL.Path, "/messages/count_tokens") {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":1}`))
			return true
		},
		shell:  edgeClaudeShell,
		result: edgeClaudeResult,
		answer: edgeClaudeAnswer,
		drive: func(ctx context.Context, _ Options, w *streamWire) error {
			if _, err := w.request(ctx, "initialize", map[string]any{}); err != nil {
				return err
			}
			return w.send(ctx, claudeUserFrame(newID(), "Local capability canary only."))
		},
		settled: func(m map[string]json.RawMessage) (bool, bool) {
			if string(m["type"]) != `"result"` {
				return false, false
			}
			var failed bool
			_ = json.Unmarshal(m["is_error"], &failed)
			return !failed && string(m["subtype"]) == `"success"`, true
		},
	},
}

func edgeCodexShell(w http.ResponseWriter, o Options, data []byte, command string) bool {
	var body struct {
		Tools []struct {
			Type, Name string
			Tools      []struct{ Type, Name string } `json:"tools"`
			Function   struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(data, &body) != nil {
		return false
	}
	name := ""
	namespace := ""
	for _, tool := range body.Tools {
		if tool.Type == "namespace" {
			for _, nested := range tool.Tools {
				if nested.Type == "function" && nested.Name == "exec_command" {
					name, namespace = nested.Name, tool.Name
					break
				}
			}
			if name != "" {
				break
			}
		}
		n := tool.Name
		if n == "" {
			n = tool.Function.Name
		}
		if n == "exec_command" || strings.HasSuffix(n, ".exec_command") {
			name = n
			break
		}
	}
	if name == "" {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	arguments, _ := json.Marshal(map[string]any{"cmd": command, "workdir": o.WorkDir, "yield_time_ms": 10000, "max_output_tokens": 8192})
	item := map[string]any{"type": "function_call", "id": "fc_edge", "call_id": "call_edge", "name": name, "arguments": string(arguments), "status": "completed"}
	if namespace != "" {
		item["namespace"] = namespace
	}
	start := map[string]any{"type": "function_call", "id": "fc_edge", "call_id": "call_edge", "name": name, "arguments": "", "status": "in_progress"}
	if namespace != "" {
		start["namespace"] = namespace
	}
	response := map[string]any{"id": "resp_shell", "object": "response", "status": "completed", "model": o.Model, "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
	writeEdgeEvents(w, []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_shell", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": start},
		{"type": "response.function_call_arguments.delta", "item_id": "fc_edge", "output_index": 0, "delta": string(arguments)},
		{"type": "response.function_call_arguments.done", "item_id": "fc_edge", "output_index": 0, "arguments": string(arguments)},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": response},
	})
	return true
}

// writeEdgeEvents streams Responses API events, each named by its type.
func writeEdgeEvents(w http.ResponseWriter, events []map[string]any) {
	for _, event := range events {
		bytes, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], bytes)
	}
}

func edgeCodexResult(data []byte) (string, bool) {
	// call_id needs its wire spelling rather than Go's default field matching.
	var generic map[string]json.RawMessage
	if json.Unmarshal(data, &generic) != nil {
		return "", false
	}
	var input []struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(generic["input"], &input) != nil {
		return "", false
	}
	for _, item := range input {
		if item.Type == "function_call_output" && item.CallID == "call_edge" {
			return edgeResultText(item.Output), true
		}
	}
	return "", false
}

func edgeCodexAnswer(w http.ResponseWriter, o Options, nonce string) {
	w.Header().Set("Content-Type", "text/event-stream")
	message := map[string]any{"id": "msg_edge", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": nonce, "annotations": []any{}}}}
	response := map[string]any{"id": "resp_edge", "object": "response", "created_at": 1, "status": "completed", "model": o.Model, "output": []any{message}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
	writeEdgeEvents(w, []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_edge", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_edge", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
		{"type": "response.content_part.added", "item_id": "msg_edge", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		{"type": "response.output_text.delta", "item_id": "msg_edge", "output_index": 0, "content_index": 0, "delta": nonce},
		{"type": "response.output_text.done", "item_id": "msg_edge", "output_index": 0, "content_index": 0, "text": nonce},
		{"type": "response.output_item.done", "output_index": 0, "item": message},
		{"type": "response.completed", "response": response},
	})
}

func edgeCodexDrive(ctx context.Context, o Options, w *streamWire) error {
	if err := codexHandshake(ctx, w, true); err != nil {
		return err
	}
	body, err := w.request(ctx, "thread/start", codexThreadParams(o, o.WorkDir, false, ""))
	if err != nil {
		return err
	}
	if err = checkCodexSandbox(o, body); err != nil {
		return err
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(body, &r) != nil || r.Thread.ID == "" {
		return ErrProtocol
	}
	_, err = w.request(ctx, "turn/start", codexTurnParams(r.Thread.ID, "Local capability canary only.", o.Effort))
	return err
}

func edgeClaudeShell(w http.ResponseWriter, o Options, data []byte, command string) bool {
	var body struct {
		Tools []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(data, &body) != nil {
		return false
	}
	found := false
	for _, tool := range body.Tools {
		found = found || tool.Name == "Bash" || tool.Name == "" && tool.Function.Name == "Bash"
	}
	if !found {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	input, _ := json.Marshal(map[string]any{"command": command, "timeout": 15000, "description": "Disposable edge canary"})
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_shell\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n", o.Model)
	io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_edge\",\"name\":\"Bash\",\"input\":{}}}\n\n")
	delta, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", delta)
	io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return true
}

func edgeClaudeResult(data []byte) (string, bool) {
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(data, &body) != nil {
		return "", false
	}
	for _, message := range body.Messages {
		var content []struct {
			Type    string          `json:"type"`
			ID      string          `json:"tool_use_id"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(message.Content, &content) != nil {
			continue
		}
		for _, item := range content {
			if item.Type == "tool_result" && item.ID == "tool_edge" {
				return edgeResultText(item.Content), true
			}
		}
	}
	return "", false
}

func edgeClaudeAnswer(w http.ResponseWriter, o Options, nonce string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_edge\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n", o.Model)
	fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", nonce)
	io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}
