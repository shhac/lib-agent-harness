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
	valid := true
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024+1))
		mu.Lock()
		requests++
		valid = valid && readErr == nil && len(data) <= 2*1024*1024 && validClaudeProbe(data, schema, cfg.Effort)
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
	if requests != 1 || !valid {
		return errors.New("Claude CLI capability check failed; update Claude Code before using constrained completion")
	}
	return nil
}

func validClaudeProbe(data, schema []byte, effort string) bool {
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
		return false
	}
	var expected, actual any
	if json.Unmarshal(schema, &expected) != nil || json.Unmarshal(request.Tools[0].Schema, &actual) != nil {
		return false
	}
	left, _ := json.Marshal(expected)
	right, _ := json.Marshal(actual)
	if !bytes.Equal(left, right) || (effort != "" && request.Output.Effort != effort) {
		return false
	}
	found := false
	for _, part := range request.System {
		if part.Type != "text" {
			return false
		}
		switch {
		case part.Text == codexInstructions:
			found = true
		case part.Text == "You are a Claude agent, built on Anthropic's Claude Agent SDK.":
		case strings.HasPrefix(part.Text, "x-anthropic-billing-header:"):
		default:
			return false
		}
	}
	return found
}
