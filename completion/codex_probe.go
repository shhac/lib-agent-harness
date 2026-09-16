package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

// probeCodex makes no inference call. A dummy provider rejects the first request
// after checking the CLI actually removed all native tools and honored identity.
func probeCodex(ctx context.Context, cfg Config, bin string, args []string, dir string, env []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("cannot start local Codex capability check")
	}
	var mu sync.Mutex
	verified := true
	requests := 0
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024+1))
		mu.Lock()
		requests++
		verified = verified && readErr == nil && len(data) <= 2*1024*1024 && validCodexProbe(data, cfg.Model, cfg.Effort)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"local capability check; no inference performed"}}`)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	probeArgs := append([]string{}, args...)
	provider := `model_providers.harness_probe={name="Harness capability check",base_url="http://` + listener.Addr().String() + `/v1",wire_api="responses",request_max_retries=0,stream_max_retries=0,env_key="HARNESS_PROBE_KEY"}`
	probeArgs = append(probeArgs, "-c", `model_provider="harness_probe"`, "-c", provider)
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Preserve the explicitly selected Codex home so the probe verifies the
	// same global-instruction boundary. OS account discovery is disposable;
	// provider auth is the explicit dummy key, never the native login.
	probeEnv := isolatedOperatingEnvironment(env, runtime.GOOS, dir)
	for _, entry := range env {
		if strings.HasPrefix(entry, "CODEX_HOME=") {
			probeEnv = append(probeEnv, entry)
		}
	}
	_, _ = runCLI(probeCtx, cfg, bin, probeArgs, dir, append(probeEnv, "HARNESS_PROBE_KEY=local-dummy-value"), `{"messages":[{"role":"user","content":"Return an empty response."}],"available_tools":[]}`)
	if err := ctx.Err(); err != nil {
		return err
	}
	mu.Lock()
	ok := verified && requests == 1
	mu.Unlock()
	if !ok {
		return errors.New("Codex capability check failed: this CLI did not prove tool-free inference with the requested model and effort; no live model call was made")
	}
	return nil
}

// validCodexProbe checks one actual outbound request body: it must parse, name
// the requested model and effort, carry no native tools, and show no sign that
// the CLI merged global AGENTS instructions into what it sends. The size bound
// belongs to the transport and stays at the handler.
func validCodexProbe(data []byte, model, effort string) bool {
	var req struct {
		Model     string            `json:"model"`
		Tools     []json.RawMessage `json:"tools"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	return json.Unmarshal(data, &req) == nil && req.Model == model && req.Reasoning.Effort == effort && len(req.Tools) == 0 && !bytes.Contains(data, []byte("# AGENTS.md instructions"))
}
