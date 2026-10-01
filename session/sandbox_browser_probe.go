package session

// One scripted app-server turn, using the real bridge and a local provider.
// The provider performs no inference and permits exactly one js call. A file
// that stayed absent is not sufficient: the result must prove the call ran and
// encountered an OS permission denial. Native startup/status alone cannot do it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const browserCanaryID = "harness_browser_canary"
const browserCanaryWrote = "browser-canary-write-succeeded"
const browserCanaryDenied = "browser-canary-denied:"

func browserCanary(path string) string {
	target, _ := json.Marshal(path)
	return `try { await (await import("node:fs/promises")).writeFile(` + string(target) + `, "x", {flag:"wx"}); nodeRepl.write("` + browserCanaryWrote + `"); } catch(e) { nodeRepl.write("` + browserCanaryDenied + `"+e.code); }`
}

func probeCodexBrowserSandbox(ctx context.Context, o Options, l *launch) error {
	fail := func() error { return browserCapability(CapabilityBrowserSandboxUnproven) }
	if l.browser == nil {
		return fail()
	}
	root, err := os.MkdirTemp("", "agent-harness-browser-proof-")
	if err != nil {
		return fail()
	}
	defer os.RemoveAll(root)
	// The sibling is outside the disposable workspace. The session's profile
	// denies temp writes, so it is outside every writable root even under /tmp.
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	for _, dir := range []string{home, workspace} {
		if os.Mkdir(dir, 0700) != nil {
			return fail()
		}
	}
	canary := filepath.Join(root, "outside-canary")
	// Establish that the same account can write this target outside confinement.
	if os.WriteFile(canary, []byte("witness"), 0600) != nil || os.Remove(canary) != nil {
		return fail()
	}
	config, err := l.browser.config(home)
	if err != nil {
		return fail()
	}
	if writePrivate(filepath.Join(home, codexConfigFile), config) != nil {
		return fail()
	}
	provider, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail()
	}
	result := make(chan string, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: browserCanaryProvider(browserCanary(canary), result)}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(provider) }()
	defer func() { _ = server.Close(); <-done }()
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	settled := make(chan struct{})
	var output string
	go func() {
		defer close(settled)
		select {
		case output = <-result:
			cancel()
		case <-probeCtx.Done():
		}
	}()
	// No caller diagnostics or additions in a probe: even installation failures
	// stay structural, and neither real credentials nor external providers exist.
	probe := o
	probe.WorkDir = workspace
	probe.OnDiagnostic = nil
	args := commandArgs(probe, newID(), false, l)
	args = append(args, "-c", `model_provider="harness_probe"`, "-c", `model_providers.harness_probe={name="Harness browser confinement check",base_url="http://`+provider.Addr().String()+`/v1",wire_api="responses",request_max_retries=0,stream_max_retries=0,env_key="HARNESS_PROBE_KEY"}`)
	env := disposableEnvironment(o, home)
	// Match the real launch's temp location, including exclusion of that path.
	for i, entry := range env {
		if strings.HasPrefix(entry, "TMPDIR=") {
			env[i] = "TMPDIR=" + sessionTempDir()
		}
	}
	_ = driveCodexProbe(probeCtx, probe, args, workspace, append(env, "HARNESS_PROBE_KEY=local-dummy-value"))
	cancel()
	<-settled
	_, statErr := os.Lstat(canary)
	return judgeBrowserSandbox(output, statErr)
}

func judgeBrowserSandbox(output string, statErr error) error {
	if statErr == nil || strings.Contains(output, browserCanaryWrote) {
		return browserCapability(CapabilityBrowserSandboxNotEnforced)
	}
	if !os.IsNotExist(statErr) {
		return browserCapability(CapabilityBrowserSandboxUnproven)
	}
	denied := false
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case browserCanaryDenied + "EPERM", browserCanaryDenied + "EACCES":
			denied = true
		}
	}
	if !denied {
		return browserCapability(CapabilityBrowserSandboxUnproven)
	}
	return nil
}

// Browser tools may be deferred; their channel inventory was checked before
// this turn. The scripted function call uses the namespace verified with
// codex-cli 0.159.2, even if the model request defers its declaration.
func browserCanaryProvider(code string, result chan<- string) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			refuseInference(w)
			return
		}
		var request struct {
			Input []struct {
				Type   string          `json:"type"`
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			} `json:"input"`
		}
		if json.Unmarshal(raw, &request) != nil {
			refuseInference(w)
			return
		}
		for _, item := range request.Input {
			if item.Type != "function_call_output" || item.CallID != browserCanaryID {
				continue
			}
			text, ok := browserCanaryOutput(item.Output)
			if !ok {
				text = ""
			}
			select {
			case result <- text:
			default:
			}
			refuseInference(w)
			return
		}
		served := false
		once.Do(func() { served = true; writeBrowserCanaryCall(w, code) })
		if !served {
			refuseInference(w)
		}
	})
}

func browserCanaryOutput(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return "", false
	}
	var out strings.Builder
	for _, part := range parts {
		if part.Type != "input_text" {
			return "", false
		}
		out.WriteString(part.Text + "\n")
	}
	return out.String(), true
}

func writeBrowserCanaryCall(w http.ResponseWriter, code string) {
	args, _ := json.Marshal(map[string]any{"code": code, "title": "Sandbox browser confinement check"})
	item := map[string]any{"type": "function_call", "namespace": "mcp__node_repl", "name": "js", "arguments": string(args), "call_id": browserCanaryID, "id": "fc_harness_browser", "status": "completed"}
	response := map[string]any{"id": "resp_harness_browser", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
	events := []any{
		map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_harness_browser", "object": "response", "status": "in_progress", "output": []any{}}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "namespace": "mcp__node_repl", "name": "js", "arguments": "", "call_id": browserCanaryID, "id": "fc_harness_browser", "status": "in_progress"}},
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_harness_browser", "output_index": 0, "delta": string(args)},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
		map[string]any{"type": "response.completed", "response": response},
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		raw, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	}
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}
