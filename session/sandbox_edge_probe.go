package session

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

var edgeProofs = struct {
	sync.Mutex
	gates map[string]chan struct{}
}{gates: map[string]chan struct{}{}}

// Internal fixture seam; every public launch uses the installed-binary probe.
var edgeProbe = probeEdge

var edgeControl = sandboxprobe.DiscoverAndProveControl

func verifyEdge(ctx context.Context, o Options, l *launch) error {
	var hosted any
	if o.Sandbox.Tools != nil {
		hosted = []any{o.Sandbox.Tools.Bridge, o.Sandbox.Tools.Tools}
	}
	payload, _ := json.Marshal([]any{edgeRevision, l.edge.identity, l.edge.plan.Key, reference(o, "").ConfigHash, o.Env, hosted})
	sum := sha256.Sum256(payload)
	key := "edge:" + hex.EncodeToString(sum[:])
	for {
		edgeProofs.Lock()
		if gate := edgeProofs.gates[key]; gate != nil {
			edgeProofs.Unlock()
			select {
			case <-gate:
				continue
			case <-ctx.Done():
				return edgeFailure(o, CapabilityProbeTimeout)
			}
		}
		if verified.holds(key) {
			edgeProofs.Unlock()
			if ctx.Err() != nil {
				return edgeFailure(o, CapabilityProbeTimeout)
			}
			return nil
		}
		gate := make(chan struct{})
		edgeProofs.gates[key] = gate
		edgeProofs.Unlock()
		probeCtx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
		err := edgeProbe(probeCtx, o)
		var failure *CapabilityError
		if probeCtx.Err() != nil && (!errors.As(err, &failure) || failure.Reason != "observed_escape") {
			err = edgeFailure(o, CapabilityProbeTimeout)
		}
		cancel()
		edgeProofs.Lock()
		if err == nil {
			verified.record(key)
		}
		delete(edgeProofs.gates, key)
		close(gate)
		edgeProofs.Unlock()
		return err
	}
}

// Inspect only the result of our exact native call, never strings in prompts,
// tool descriptions or the command itself, which also contain canary markers.
func edgeNativeResult(engine harness.Engine, data []byte) (string, bool) {
	var body struct {
		Input []struct {
			Type, CallID string
			Output       json.RawMessage `json:"output"`
		} `json:"input"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	// call_id needs its wire spelling rather than Go's default field matching.
	var generic map[string]json.RawMessage
	if json.Unmarshal(data, &generic) != nil {
		return "", false
	}
	if engine == harness.Codex {
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
	} else {
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
	}
	return "", false
}

func edgeResultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, block := range blocks {
			b.WriteString(block.Text)
			b.WriteByte('\n')
		}
		return b.String()
	}
	return ""
}

func edgeShellResponse(w http.ResponseWriter, o Options, data []byte, command string) bool {
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
		if o.Provider.Engine == harness.Codex && tool.Type == "namespace" {
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
		if o.Provider.Engine == harness.Claude && n == "Bash" || o.Provider.Engine == harness.Codex && (n == "exec_command" || strings.HasSuffix(n, ".exec_command")) {
			name = n
			break
		}
	}
	if name == "" {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if o.Provider.Engine == harness.Claude {
		input, _ := json.Marshal(map[string]any{"command": command, "timeout": 15000, "description": "Disposable edge canary"})
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_shell\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n", o.Model)
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_edge\",\"name\":\"Bash\",\"input\":{}}}\n\n")
		delta, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", delta)
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	} else {
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
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_shell", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.added", "output_index": 0, "item": start},
			{"type": "response.function_call_arguments.delta", "item_id": "fc_edge", "output_index": 0, "delta": string(arguments)},
			{"type": "response.function_call_arguments.done", "item_id": "fc_edge", "output_index": 0, "arguments": string(arguments)},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": response},
		} {
			bytes, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], bytes)
		}
	}
	return true
}

const edgeCanaryHost = "edge-canary.invalid"

func probeEdge(ctx context.Context, o Options) error {
	root, err := os.MkdirTemp("", "harness-edge-canary-")
	if err != nil {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	probe := o
	probe.WorkDir = filepath.Join(root, "work")
	probe.RuntimeHome = filepath.Join(root, "runtime")
	s := *o.Sandbox
	e := *s.Edge
	e.ClaudeToken = "sk-ant-oat01-dummy-edge-setup-token-not-a-login"
	s.Edge = &e
	probe.Sandbox = &s
	// Keep the selected login deny selector identical to production. The child
	// uses only the disposable runtime auth/config through the explicit env.
	for _, dir := range []string{probe.WorkDir, probe.RuntimeHome, filepath.Join(probe.RuntimeHome, "tmp"), filepath.Join(probe.RuntimeHome, "claude"), filepath.Join(probe.WorkDir, ".git"), filepath.Join(root, "outside")} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return edgeFailure(o, CapabilityEdgeConfinementFailed)
		}
	}
	if err = os.WriteFile(filepath.Join(probe.RuntimeHome, "auth.json"), []byte(`{"OPENAI_API_KEY":"dummy-edge-key"}`), 0600); err != nil {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	var host *toolHost
	if s.Tools != nil {
		tools := *s.Tools
		tools.Dir = filepath.Join(root, "tools")
		if err = os.MkdirAll(tools.Dir, 0700); err != nil {
			return edgeFailure(o, CapabilityHostedToolsMissing)
		}
		host, err = newToolHost(tools, nil)
		if err != nil {
			return edgeFailure(o, CapabilityHostedToolsMissing)
		}
		host.setProbing(true)
		host.edge = true
		defer host.close()
		s.Tools = &tools
	}
	names := []string{edgeCanaryHost}
	for _, d := range edgeDestinations(o) {
		names = append(names, d.Host)
	}
	certificate, certPEM, err := edgeCertificate(names...)
	if err != nil {
		return edgeFailure(o, CapabilityEdgeProviderPath)
	}
	caPath := filepath.Join(probe.RuntimeHome, "canary-ca.pem")
	if err = os.WriteFile(caPath, certPEM, 0600); err != nil {
		return edgeFailure(o, CapabilityEdgeProviderPath)
	}
	nonce := "edge-provider-" + newID()
	var requested atomic.Bool
	var nativeMu sync.Mutex
	var nativeFailure error
	var nativeProved bool
	scriptPath := filepath.Join(probe.WorkDir, "edge-canary.pl")
	if err = os.WriteFile(scriptPath, []byte(edgeBoundaryScript), 0600); err != nil {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	command := "/usr/bin/perl " + sandboxprobe.ShellQuote(scriptPath)
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if r.Method != http.MethodPost {
			http.Error(w, "fake provider only", 404)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || !json.Valid(data) {
			http.Error(w, "invalid fake request", 400)
			return
		}
		if o.Provider.Engine == harness.Claude && strings.HasSuffix(r.URL.Path, "/messages/count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":1}`))
			return
		}
		requested.Store(true)
		output, hasResult := edgeNativeResult(o.Provider.Engine, data)
		if hasResult {
			failure := judgeEdgeBoundary(probe, []byte(output), nil)
			nativeMu.Lock()
			var escaped *CapabilityError
			if failure != nil && (nativeFailure == nil || (errors.As(failure, &escaped) && escaped.Reason == "observed_escape")) {
				nativeFailure = failure
			}
			failure = nativeFailure
			nativeProved = failure == nil
			nativeMu.Unlock()
			if failure != nil {
				http.Error(w, "native edge canary refused", 400)
				return
			}
		} else {
			if !edgeShellResponse(w, o, data, command) {
				nativeMu.Lock()
				if nativeFailure == nil {
					nativeFailure = edgeFailure(o, CapabilityEdgeConfinementFailed)
				}
				nativeMu.Unlock()
				http.Error(w, "required native shell surface absent", 400)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if o.Provider.Engine == harness.Claude {
			fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_edge\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n", o.Model)
			fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", nonce)
			io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			message := map[string]any{"id": "msg_edge", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": nonce, "annotations": []any{}}}}
			response := map[string]any{"id": "resp_edge", "object": "response", "created_at": 1, "status": "completed", "model": o.Model, "output": []any{message}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": "resp_edge", "status": "in_progress", "output": []any{}}},
				{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_edge", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
				{"type": "response.content_part.added", "item_id": "msg_edge", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
				{"type": "response.output_text.delta", "item_id": "msg_edge", "output_index": 0, "content_index": 0, "delta": nonce},
				{"type": "response.output_text.done", "item_id": "msg_edge", "output_index": 0, "content_index": 0, "text": nonce},
				{"type": "response.output_item.done", "output_index": 0, "item": message},
				{"type": "response.completed", "response": response},
			} {
				bytes, _ := json.Marshal(event)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], bytes)
			}
		}
	}))
	provider.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	provider.StartTLS()
	defer provider.Close()
	p, err := edgeproxy.NewCanary(edgeDestinations(o), edgeCanaryHost, provider.Listener.Addr().String())
	if err != nil {
		return edgeFailure(o, CapabilityEdgeProviderPath)
	}
	defer p.Close()
	socket := ""
	if host != nil {
		socket = host.socket
	}
	plan, err := edgePlan(probe, p, socket)
	if err != nil {
		return err
	}
	plan.Env = append(plan.Env, "SSL_CERT_FILE="+caPath, "NODE_EXTRA_CA_CERTS="+caPath)
	boundary, closeBoundary, err := prepareEdgeBoundary(ctx, probe, p, root)
	if err != nil {
		return err
	}
	defer closeBoundary()
	plan.Env = append(plan.Env, boundary.Env...)
	modes := []bool{false}
	if o.Provider.Engine == harness.Codex && o.Sandbox.Edge.BaseURL == "" {
		modes = append(modes, true)
	}
	for _, subscription := range modes {
		if subscription {
			claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix(), "email": "dummy@example.invalid", "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "dummy-edge-account", "chatgpt_plan_type": "plus"}})
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".dummy"
			auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"id_token": jwt, "access_token": jwt, "refresh_token": "dummy-refresh", "account_id": "dummy-edge-account"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
			if err = os.WriteFile(filepath.Join(probe.RuntimeHome, "auth.json"), auth, 0600); err != nil {
				return edgeFailure(o, CapabilityEdgeProviderPath)
			}
		}
		requested.Store(false)
		nativeMu.Lock()
		nativeProved = false
		nativeMu.Unlock()
		if err = driveEdgeInference(ctx, probe, plan.Binary, plan.Args, plan.Env, nonce, host); err != nil || !requested.Load() {
			nativeMu.Lock()
			failure := nativeFailure
			nativeMu.Unlock()
			if failure != nil {
				return failure
			}
			if ctx.Err() != nil {
				return edgeFailure(o, CapabilityProbeTimeout)
			}
			return edgeFailure(o, CapabilityEdgeProviderPath)
		}
		nativeMu.Lock()
		proved := nativeProved
		nativeMu.Unlock()
		if !proved {
			return edgeFailure(o, CapabilityEdgeConfinementFailed)
		}
	}
	if host != nil && !host.served() {
		return edgeFailure(o, CapabilityHostedToolsMissing)
	}
	return nil
}

func edgeCertificate(names ...string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: edgeCanaryHost}, DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
	pair, err := tls.X509KeyPair(cert, priv)
	return pair, cert, err
}

func driveEdgeInference(ctx context.Context, o Options, binary string, prefix, env []string, nonce string, host *toolHost) error {
	l := &launch{extra: edgeArgs(o)}
	if host != nil {
		hosted, err := engines[o.Provider.Engine].sandbox.hostTools(host)
		if err != nil {
			return err
		}
		l.extra = append(l.extra, hosted...)
	}
	args := commandArgs(o, newID(), false, l)
	wrapped := o
	wrapped.Provider.CLI.Binary = binary
	wrapped.OnDiagnostic = nil
	var observed atomic.Bool
	completed := make(chan bool, 1)
	w, err := newProcessWireArgs(ctx, wrapped, append(append([]string(nil), prefix...), args...), env, nil, func(m map[string]json.RawMessage) {
		bytes, _ := json.Marshal(m)
		if strings.Contains(string(bytes), nonce) {
			observed.Store(true)
		}
		if o.Provider.Engine == harness.Codex {
			if string(m["method"]) == `"turn/completed"` {
				var v struct {
					Turn struct {
						Status string `json:"status"`
					} `json:"turn"`
				}
				_ = json.Unmarshal(m["params"], &v)
				select {
				case completed <- v.Turn.Status == "completed":
				default:
				}
			}
		} else if string(m["type"]) == `"result"` {
			var failed bool
			_ = json.Unmarshal(m["is_error"], &failed)
			select {
			case completed <- !failed && string(m["subtype"]) == `"success"`:
			default:
			}
		}
	}, func(error) {})
	if err != nil {
		return err
	}
	defer func() { w.close(); <-w.reaped }()
	if o.Provider.Engine == harness.Codex {
		if err = codexHandshake(ctx, w, true); err != nil {
			return err
		}
		body, e := w.request(ctx, "thread/start", codexThreadParams(o, o.WorkDir, false, ""))
		if e != nil {
			return e
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
	} else {
		_, err = w.request(ctx, "initialize", map[string]any{})
		if err == nil {
			err = w.send(ctx, claudeUserFrame(newID(), "Local capability canary only."))
		}
	}
	if err != nil {
		return err
	}
	select {
	case success := <-completed:
		if !success || !observed.Load() {
			return ErrProtocol
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.reaped:
		return ErrTransport
	}
}

func prepareEdgeBoundary(ctx context.Context, o Options, p *edgeproxy.Proxy, root string) (sandboxbridge.EdgePlan, func(), error) {
	fail := func(code string) (sandboxbridge.EdgePlan, func(), error) {
		return sandboxbridge.EdgePlan{}, func() {}, edgeFailure(o, code)
	}
	// Controls contain no credentials or caller payloads. An unavailable
	// off-machine witness refuses proof; a timeout is never counted as denial.
	target, tcp443, err := edgeNetworkControls(ctx, o)
	if err != nil {
		return sandboxbridge.EdgePlan{}, func() {}, err
	}
	witness, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fail(CapabilityEdgeControlUnavailable)
	}
	failure := func(code string) (sandboxbridge.EdgePlan, func(), error) { witness.Close(); return fail(code) }
	go func() {
		for {
			conn, e := witness.Accept()
			if e != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, _ = conn.Write([]byte("witness"))
			conn.Close()
		}
	}()
	outside := filepath.Join(root, "outside")
	for path, data := range map[string][]byte{filepath.Join(outside, "secret"): []byte("outside-marker"), filepath.Join(o.WorkDir, "inside"): []byte("inside-marker"), filepath.Join(o.WorkDir, ".git", "config"): []byte("git-marker")} {
		if err = os.WriteFile(path, data, 0600); err != nil {
			return failure(CapabilityEdgeConfinementFailed)
		}
	}
	if err = os.MkdirAll(filepath.Join(o.WorkDir, ".claude"), 0700); err != nil {
		return failure(CapabilityEdgeConfinementFailed)
	}
	for _, name := range []string{"claude/agents", "claude/commands", "claude/skills", "prompts", "rules"} {
		dir := filepath.Join(o.RuntimeHome, name)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return failure(CapabilityEdgeConfinementFailed)
		}
		if err = os.WriteFile(filepath.Join(dir, "instruction.md"), []byte("fixture instruction"), 0600); err != nil {
			return failure(CapabilityEdgeConfinementFailed)
		}
	}
	for path, data := range map[string]string{filepath.Join(o.WorkDir, ".claude", "settings.json"): "{}", filepath.Join(o.WorkDir, ".mcp.json"): "{}", filepath.Join(o.RuntimeHome, "config.toml"): "# protected fixture\n", filepath.Join(o.RuntimeHome, "login.shared"): "{}"} {
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			return failure(CapabilityEdgeConfinementFailed)
		}
	}
	executable, err := os.ReadFile("/bin/echo")
	if err != nil {
		return failure(CapabilityEdgeConfinementFailed)
	}
	if err = os.WriteFile(filepath.Join(outside, "exec"), executable, 0700); err != nil {
		return failure(CapabilityEdgeConfinementFailed)
	}
	plan, err := edgePlan(o, p, "")
	if err != nil {
		witness.Close()
		return sandboxbridge.EdgePlan{}, func() {}, err
	}
	plan.Env = append(plan.Env, "EDGE_OUTSIDE="+outside, "EDGE_CONTROL="+target.Addr, "EDGE_TCP443="+tcp443, "EDGE_WITNESS="+witness.Addr().String())
	u, _ := url.Parse(p.Endpoint())
	plan.Env = append(plan.Env, "EDGE_PROXY="+u.Host, "EDGE_PROXY_AUTH="+u.User.String(), fmt.Sprintf("EDGE_WRITE=%t", o.Sandbox.Write), fmt.Sprintf("EDGE_LOOPBACK=%t", o.Sandbox.Loopback))
	return plan, func() { _ = witness.Close() }, nil
}

const edgeBoundaryScript = `
use strict; use warnings; use Socket; use IO::Socket::INET; use MIME::Base64; use Errno qw(EPERM EACCES);
sub mark { print "$_[0]\n"; }
sub denied { return $! == EPERM || $! == EACCES; }
my $outside=$ENV{EDGE_OUTSIDE};
if (open(my $f,'<','inside')) { local $/; my $v=<$f>; mark('read-inside') if $v eq 'inside-marker'; }
if (open(my $f,'<',"$outside/secret")) { mark('escape-read'); } elsif (denied()) { mark('read-outside-denied'); }
if (open(my $f,'>',"$outside/write")) { mark('escape-write'); } elsif (denied()) { mark('write-outside-denied'); }
if (open(my $f,'>','write-inside')) { print $f 'ok'; close $f; mark('write-inside'); } elsif (denied()) { mark('write-inside-denied'); }
if (open(my $f,'>>','.git/config')) { mark('escape-git'); } elsif (denied()) { mark('git-denied'); }
for my $pair (['.claude/settings.json','claude-config-denied'],['.mcp.json','mcp-config-denied'],["$ENV{HOME}/config.toml",'runtime-config-denied'],["$ENV{HOME}/auth.json",'runtime-auth-denied'],["$ENV{HOME}/login.shared",'runtime-record-denied']) {
  if(open(my $f,'>',$pair->[0])) { mark('escape-protected-state'); } elsif(denied()) {mark($pair->[1]);}
}
for my $dir ('claude/agents','claude/commands','claude/skills','prompts','rules') {
  my $mark=$dir; $mark =~ s!/!-!g;
  if(open(my $f,'>',"$ENV{HOME}/$dir/instruction.md")){mark('escape-runtime-loader');}elsif(denied()){mark("loader-$mark-denied");}
}
my $child=fork(); die 'fork' unless defined $child;
if ($child==0) { exec("$outside/exec",'x'); exit(denied()?42:43); }
waitpid($child,0); mark(($?>>8)==42?'exec-outside-denied':'escape-exec');
# An allowed interpreter in a grandchild still cannot read outside the set.
my $code='use Errno qw(EPERM EACCES); if (open(my $f,"<",$ENV{EDGE_OUTSIDE}."/secret")){exit 9;} exit(($!==EPERM||$!==EACCES)?42:43);';
system('/bin/sh','-c','exec /usr/bin/perl -e "$1"','edge-grandchild',$code);
mark(($?>>8)==42?'grandchild-denied':'escape-grandchild');
for my $port (443,53) {
 mark("control-tcp$port=".($port==443?$ENV{EDGE_TCP443}:$ENV{EDGE_CONTROL}));
  my $s=IO::Socket::INET->new(PeerAddr=>($port==443?$ENV{EDGE_TCP443}:$ENV{EDGE_CONTROL}),PeerPort=>$port,Proto=>'tcp',Timeout=>2);
  if($s){mark("escape-network-tcp$port");close $s;} elsif(denied()){mark("tcp$port-denied");}
}
socket(my $udp,PF_INET,SOCK_DGRAM,0) or die 'udp';
my $v=send($udp,'probe',0,sockaddr_in(53,inet_aton($ENV{EDGE_CONTROL})));
if(defined $v){mark('escape-network-udp53');}elsif(denied()){mark('udp53-denied');} close $udp;
my @dns=gethostbyname('example.com'); mark(@dns?'escape-dns':'dns-denied');
my($ph,$pp)=split(/:/,$ENV{EDGE_PROXY});
my $proxy=IO::Socket::INET->new(PeerAddr=>$ph,PeerPort=>$pp,Proto=>'tcp',Timeout=>2) or die 'proxy';
my $auth=encode_base64($ENV{EDGE_PROXY_AUTH},'');
print $proxy "CONNECT forbidden-edge.invalid:443 HTTP/1.1\r\nHost: forbidden-edge.invalid:443\r\nProxy-Authorization: Basic $auth\r\n\r\n";
my $status=<$proxy>; mark('proxy-denied') if defined($status) && $status =~ /^HTTP\/1[.]1 403 /; close $proxy;
my($wh,$wp)=split(/:/,$ENV{EDGE_WITNESS});
my $host=IO::Socket::INET->new(PeerAddr=>$wh,PeerPort=>$wp,Proto=>'tcp',Timeout=>2);
if($host){my $buf='';read($host,$buf,7);mark('host-reach') if $buf eq 'witness';close $host;}elsif(denied()){mark('host-denied');}
if($ENV{EDGE_LOOPBACK} eq 'true'){
  my $server=IO::Socket::INET->new(LocalAddr=>'127.0.0.1',LocalPort=>0,Listen=>1,Proto=>'tcp') or die 'bind';
  my $client=IO::Socket::INET->new(PeerAddr=>'127.0.0.1',PeerPort=>$server->sockport,Proto=>'tcp',Timeout=>2) or die 'reach';
  my $accepted=$server->accept() or die 'accept';print $client 'self';my $buf='';read($accepted,$buf,4);mark('bind-reach') if $buf eq 'self';close $client;close $accepted;close $server;
}
mark('finished');
`

func judgeEdgeBoundary(o Options, out []byte, runErr error) error {
	lines := map[string]bool{}
	for _, line := range strings.Fields(string(out)) {
		lines[line] = true
	}
	// An observed escape outranks silence, malformed output and interruption.
	for key := range lines {
		if strings.HasPrefix(key, "escape-network") {
			return &CapabilityError{Engine: o.Provider.Engine, Code: CapabilityEdgeEgressNotRefused, Phase: BeforeLaunch, Reason: "observed_escape"}
		}
		if strings.HasPrefix(key, "escape-dns") {
			return &CapabilityError{Engine: o.Provider.Engine, Code: CapabilityEdgeDNSNotRefused, Phase: BeforeLaunch, Reason: "observed_escape"}
		}
		if strings.HasPrefix(key, "escape-") {
			return &CapabilityError{Engine: o.Provider.Engine, Code: CapabilityEdgeConfinementFailed, Phase: BeforeLaunch, Reason: "observed_escape"}
		}
	}
	for _, key := range []string{"read-inside", "read-outside-denied", "write-outside-denied", "exec-outside-denied", "grandchild-denied", "git-denied", "claude-config-denied", "mcp-config-denied", "runtime-config-denied", "runtime-auth-denied", "runtime-record-denied", "loader-claude-agents-denied", "loader-claude-commands-denied", "loader-claude-skills-denied", "loader-prompts-denied", "loader-rules-denied", "proxy-denied", "tcp443-denied", "tcp53-denied", "udp53-denied", "dns-denied", "finished"} {
		if !lines[key] {
			if strings.HasPrefix(key, "tcp") || strings.HasPrefix(key, "udp") {
				return edgeFailure(o, CapabilityEdgeEgressNotRefused)
			}
			if key == "dns-denied" {
				return edgeFailure(o, CapabilityEdgeDNSNotRefused)
			}
			return edgeFailure(o, CapabilityEdgeConfinementFailed)
		}
	}
	if runErr != nil {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	if o.Sandbox.Write && !lines["write-inside"] || !o.Sandbox.Write && !lines["write-inside-denied"] {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	if o.Sandbox.Loopback && (!lines["bind-reach"] || !lines["host-reach"]) || !o.Sandbox.Loopback && !lines["host-denied"] {
		return edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	return nil
}
