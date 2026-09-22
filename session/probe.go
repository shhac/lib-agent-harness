package session

// The capability probe: one synthetic turn against a loopback provider that
// refuses inference, and the judgement of what the harness actually sent.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// probeRestriction drives one synthetic turn against a provider that refuses
// every request, then inspects what the harness actually sent. A refusal means
// no inference happens, so no agent loop starts and no tool can be called; the
// tool host refuses calls for the probe's lifetime regardless.
func probeRestriction(ctx context.Context, o Options, l *launch) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeFailed, Phase: BeforeLaunch}
	}
	var mu sync.Mutex
	captured := [][]byte{}
	arrived := make(chan struct{}, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		mu.Lock()
		if readErr == nil {
			captured = append(captured, data)
		} else {
			captured = append(captured, nil)
		}
		mu.Unlock()
		select {
		case arrived <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"local capability check; no inference performed"}}`)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()

	dir, err := os.MkdirTemp("", "agent-harness-probe-")
	if err != nil {
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeFailed, Phase: BeforeLaunch}
	}
	defer os.RemoveAll(dir)
	probeCtx, cancel := context.WithTimeout(ctx, o.Restriction.Probe)
	defer cancel()
	// The question is answered the moment a request arrives. Wait a short while
	// afterwards in case the harness retries with a different surface, then stop:
	// a proved configuration should not sit out the whole timeout.
	go func() {
		select {
		case <-arrived:
			select {
			case <-time.After(probeSettle):
			case <-probeCtx.Done():
			}
			cancel()
		case <-probeCtx.Done():
		}
	}()
	runErr := driveProbe(probeCtx, o, l, dir, listener.Addr().String())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	mu.Lock()
	requests := append([][]byte(nil), captured...)
	mu.Unlock()
	if len(requests) == 0 {
		if probeCtx.Err() != nil {
			return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeTimeout, Phase: BeforeLaunch}
		}
		if runErr != nil {
			return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeFailed, Phase: BeforeLaunch}
		}
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeNoRequest, Phase: BeforeLaunch}
	}
	hosted := toolNames(o.Restriction.Tools.Tools)
	var surfaces []requestSurface
	for _, body := range requests {
		if body == nil {
			return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeUnreadable, Phase: BeforeLaunch}
		}
		surface, ok := readSurface(body, o.Restriction.Tools.Server)
		if !ok {
			return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeUnreadable, Phase: BeforeLaunch}
		}
		surfaces = append(surfaces, surface)
		if failure := inspectRequestIdentity(o, body); failure != nil {
			return failure
		}
	}
	// A harness that defers its MCP tools never puts them in a request, so the
	// tool channel's own record of having served them is the positive evidence.
	//
	// Assign before returning: a typed nil pointer returned straight into an
	// error result is not nil, and every passing check would read as a failure.
	if failure := judgeSurfaces(string(o.Engine), hosted, surfaces, l.host.served()); failure != nil {
		return failure
	}
	return nil
}

// inspectRequestIdentity checks the things that are about the request rather
// than its tool surface: that the selected model and effort survived, and that
// no inherited instruction material was merged in.
func inspectRequestIdentity(o Options, body []byte) *CapabilityError {
	if o.Engine != Codex {
		return nil
	}
	var request struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(body, &request) != nil {
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeUnreadable, Phase: BeforeLaunch}
	}
	// Auxiliary requests need not name the model; only a request that does is
	// evidence about it.
	if request.Model != "" && request.Model != o.Model {
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityChangedModel, Phase: BeforeLaunch}
	}
	if o.Effort != "" && request.Reasoning.Effort != "" && request.Reasoning.Effort != o.Effort {
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityChangedEffort, Phase: BeforeLaunch}
	}
	if bytes.Contains(body, []byte("# AGENTS.md instructions")) {
		return &CapabilityError{Engine: string(o.Engine), Code: CapabilityInstructionsMerged, Phase: BeforeLaunch}
	}
	return nil
}

// probeSettle is how long the check keeps listening after the harness's first
// request, in case it retries with a different surface.
const probeSettle = 1500 * time.Millisecond

// driveProbe starts the harness with exactly the arguments the real session
// would use, and asks it for one turn. Equivalence is the whole point: a check
// run with different permission modes, instructions or inputs would establish
// something about a configuration nobody is going to launch. Only the home, the
// credential and the provider endpoint differ, and all three are the reason the
// check is safe to run.
func driveProbe(ctx context.Context, o Options, l *launch, dir, endpoint string) error {
	env := disposableEnvironment(o, dir)
	id := newID()
	args := commandArgs(o, id, false, l)
	if o.Engine == Claude {
		env = append(env, "ANTHROPIC_API_KEY=agent-harness-local-probe", "ANTHROPIC_BASE_URL=http://"+endpoint, "MAX_RETRIES=0", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1")
		return driveClaudeProbe(ctx, o, args, id, dir, env)
	}
	provider := `model_providers.harness_probe={name="Harness capability check",base_url="http://` + endpoint + `/v1",wire_api="responses",request_max_retries=0,stream_max_retries=0,env_key="HARNESS_PROBE_KEY"}`
	args = append(args, "-c", `model_provider="harness_probe"`, "-c", provider)
	return driveCodexProbe(ctx, o, args, dir, append(env, "HARNESS_PROBE_KEY=local-dummy-value"))
}

func driveClaudeProbe(ctx context.Context, o Options, args []string, id, dir string, env []string) error {
	prompt, _ := json.Marshal(claudeUserFrame(id, "Capability check only."))
	cmd, p, err := process.Command(ctx, o.Binary, args...)
	if err != nil {
		return err
	}
	defer p.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(append(prompt, '\n'))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	return p.Run()
}

// driveCodexProbe speaks the app-server protocol the real session uses, so the
// surface it proves is the one the session will run with. The turn it starts is
// expected to fail: the provider refuses it. Only what was sent matters.
func driveCodexProbe(ctx context.Context, o Options, args []string, dir string, env []string) error {
	w, err := newProcessWireArgs(ctx, o, args, env, nil, func(map[string]json.RawMessage) {}, func(error) {})
	if err != nil {
		return err
	}
	defer func() { w.close(); <-w.reaped }()
	if err = codexHandshake(ctx, w); err != nil {
		return err
	}
	body, err := w.request(ctx, "thread/start", codexThreadParams(o, dir, false, ""))
	if err != nil {
		return err
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(body, &started) != nil || started.Thread.ID == "" {
		return ErrProtocol
	}
	turn := codexTurnParams(started.Thread.ID, "Capability check only.", o.Effort)
	// turn/start is acknowledged before the harness contacts its provider, so
	// returning here would close the transport during the very request the check
	// exists to read. Wait instead: the caller cancels this context as soon as a
	// request has been captured, or when the check's own bound expires.
	go func() { _, _ = w.request(ctx, "turn/start", turn) }()
	<-ctx.Done()
	return nil
}

func toolNames(tools []ToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}
