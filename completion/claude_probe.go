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

// probeClaude verifies the installed CLI's outbound tool and instruction surface
// against a local rejecting provider. Only a synthetic prompt and dummy API key
// are used. A CLI upgrade cannot silently enable native execution in a constrained caller.
func probeClaude(ctx context.Context, cfg Config, bin string, args []string, dir string, env []string, schema []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("cannot start local Claude capability check")
	}
	var mu sync.Mutex
	requests := 0
	preflights := 0
	mismatch := ""
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Claude Code performs a bodyless endpoint discovery check before
		// inference. It is not a model request and cannot establish capability.
		if r.Method == http.MethodHead && r.URL.Path == "/api/hello" {
			mu.Lock()
			preflights++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024+1))
		mu.Lock()
		requests++
		if mismatch == "" {
			if readErr != nil || len(data) > 2*1024*1024 {
				mismatch = "invalid or oversized request"
			} else {
				mismatch = claudeProbeMismatch(data, schema, cfg.Effort)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"local capability probe; no inference performed"}}`)
	})}
	go server.Serve(listener)
	defer server.Close()
	// Dummy auth and rebased OS directories prevent native account discovery,
	// including Windows USERPROFILE/APPDATA/LOCALAPPDATA and macOS USER.
	probeEnv := isolatedOperatingEnvironment(env, runtime.GOOS, dir)
	probeEnv = append(probeEnv, "CLAUDE_CONFIG_DIR="+dir, "ANTHROPIC_API_KEY=agent-harness-local-probe", "ANTHROPIC_BASE_URL=http://"+listener.Addr().String())
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = runCLI(probeCtx, cfg, bin, args, dir, probeEnv, `{"messages":[{"role":"user","content":"Capability check only."}],"available_tools":[]}`)
	if err := ctx.Err(); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if probeCtx.Err() != nil {
		return errors.New("Claude CLI capability check timed out against the local test provider; no account inference was attempted")
	}
	if requests == 0 {
		return errors.New("Claude CLI capability check made no request to the local test provider; check CLI startup and supported flags (no account inference attempted)")
	}
	// Claude may retry a rejected request using a compatibility fallback, even
	// with MAX_RETRIES=0. Every request must still prove the same restricted
	// tool, schema, system instruction and effort surface. Bound local retries.
	if requests > 4 || preflights > 4 {
		return errors.New("Claude CLI capability check exceeded its local request limit; no account inference was attempted")
	}
	if mismatch != "" {
		return errors.New("Claude CLI capability check rejected " + mismatch + "; constrained completion remains disabled (not an account login check)")
	}
	return nil
}

func validClaudeProbe(data, schema []byte, effort string) bool {
	return claudeProbeMismatch(data, schema, effort) == ""
}

func claudeProbeMismatch(data, schema []byte, effort string) string {
	var request struct {
		Tools []struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
		Output struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if json.Unmarshal(data, &request) != nil || len(request.Tools) != 1 || request.Tools[0].Name != "StructuredOutput" {
		return "unexpected tools"
	}
	var expected, actual any
	if json.Unmarshal(schema, &expected) != nil || json.Unmarshal(request.Tools[0].Schema, &actual) != nil {
		return "invalid output schema"
	}
	left, _ := json.Marshal(expected)
	right, _ := json.Marshal(actual)
	if !bytes.Equal(left, right) {
		return "changed output schema"
	}
	if effort != "" && request.Output.Effort != effort {
		return "changed reasoning effort"
	}
	found := false
	for _, part := range request.System {
		if part.Type != "text" {
			return "unexpected system instruction type"
		}
		switch {
		case part.Text == codexInstructions:
			found = true
		case part.Text == "You are a Claude agent, built on Anthropic's Claude Agent SDK.":
		case strings.HasPrefix(part.Text, "x-anthropic-billing-header:"):
		default:
			return "unexpected system instructions"
		}
	}
	if !found {
		return "missing application instructions"
	}
	return ""
}
