package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/internal/restrict"
	"github.com/shhac/lib-agent-harness/process"
)

// prepareLaunch builds a restricted session's runtime and proves it before any
// credentialed process exists. Order is the point: the tool channel and the
// restricted arguments are assembled, checked against the installed harness
// with a disposable login and a provider that refuses to infer, and only then
// handed to the real launch. A caller's login is never present while the
// question "does this build actually drop its own tools" is still open.
func prepareLaunch(ctx context.Context, o Options) (*launch, error) {
	if o.Restriction == nil {
		return nil, nil
	}
	host, err := newToolHost(o.Restriction.Tools)
	if err != nil {
		return nil, err
	}
	l := &launch{host: host}
	fail := func(err error) (*launch, error) { host.close(); return nil, err }
	if o.Engine == Claude {
		l.extra = claudeRestrictedArgs(host)
	} else {
		if inherited := restrict.InspectCodexHome(o.Home); inherited != nil {
			var named *restrict.InheritedConfig
			code := CapabilityInheritedConfig
			tools := []string{}
			if errors.As(inherited, &named) {
				tools = append(tools, named.Key)
			}
			return fail(&CapabilityError{Engine: string(Codex), Code: code, Phase: BeforeLaunch, Tools: tools})
		}
		catalog, readErr := readCodexCatalog(ctx, o)
		if readErr != nil {
			return fail(readErr)
		}
		if o.Effort == "" {
			o.Effort = restrict.CodexCatalogEffort(catalog, o.Model)
		}
		restricted, restrictErr := restrictedCatalogFor(catalog, o.Model, o.Effort)
		if restrictErr != nil {
			return fail(restrictErr)
		}
		l.catalog = filepath.Join(host.cfg.Dir, "catalog.json")
		if err = writePrivate(l.catalog, restricted); err != nil {
			return fail(err)
		}
		if l.extra, err = codexRestrictedArgs(host, l.catalog); err != nil {
			return fail(err)
		}
	}
	// The probe is unconditional. What can be skipped is repeating it for a
	// binary and an argument set already proved in this process — which is a
	// record of evidence, not an assertion that evidence was unnecessary.
	key, err := verificationKey(o, l)
	if err != nil {
		return fail(err)
	}
	if verified.holds(key) {
		return l, nil
	}
	host.setProbing(true)
	err = probeRestriction(ctx, o, l)
	host.setProbing(false)
	if err != nil {
		return fail(err)
	}
	verified.record(key)
	return l, nil
}

// verificationKey identifies exactly what a probe established: this binary, as
// it is on disk right now, launched with these arguments. Anything else — a
// different binary, an upgraded one, a changed tool surface — is a different
// question and gets asked again.
//
// The channel's ephemeral paths are excluded, because they change per launch
// and are not part of what the probe judged. The tool identifiers are included,
// because they are.
func verificationKey(o Options, l *launch) (string, error) {
	info, err := os.Stat(o.Binary)
	if err != nil {
		return "", &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeFailed, Phase: BeforeLaunch}
	}
	stable := make([]string, 0, len(l.extra))
	for _, arg := range l.extra {
		if strings.Contains(arg, l.host.socketDir) || strings.Contains(arg, l.host.cfg.Dir) {
			continue
		}
		stable = append(stable, arg)
	}
	payload, _ := json.Marshal(struct {
		Engine                Engine
		Binary, Model, Effort string
		Size                  int64
		Modified              time.Time
		Args, Tools           []string
		Instructions          Instructions
		Policy                Policy
	}{o.Engine, o.Binary, o.Model, o.Effort, info.Size(), info.ModTime(), stable, o.Restriction.Tools.Qualified(), o.Instructions, o.Policy})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// verified remembers capability checks for this process only. Nothing is
// written to disk: a restart re-proves, because a restart is exactly when an
// installed CLI is most likely to have changed underneath.
var verified = &verificationCache{seen: map[string]bool{}}

type verificationCache struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (c *verificationCache) holds(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}
func (c *verificationCache) record(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > 64 {
		c.seen = map[string]bool{}
	}
	c.seen[key] = true
}

// readCodexCatalog reads the installed model catalog with a disposable home and
// no credentials. It creates no thread and performs no inference.
func readCodexCatalog(ctx context.Context, o Options) ([]byte, error) {
	dir, err := os.MkdirTemp("", "agent-harness-catalog-")
	if err != nil {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogUnavailable, Phase: BeforeLaunch}
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runOnce(ctx, o.Binary, []string{"debug", "models", "--bundled"}, dir, disposableEnvironment(o, dir))
	if err != nil {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogUnavailable, Phase: BeforeLaunch}
	}
	return out, nil
}

// disposableEnvironment strips the operating environment down to what a CLI
// needs to start, with its home pointed at a throwaway directory. No login, no
// provider credential and no account identity is reachable from it.
func disposableEnvironment(o Options, dir string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "HOME=" + dir, "TMPDIR=" + dir}
	if o.Engine == Codex {
		return append(env, "CODEX_HOME="+dir)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+dir)
}

func runOnce(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	out := &boundedBuffer{limit: 4 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	p, err := process.New(cmd)
	if err != nil {
		return nil, err
	}
	cmd.Cancel = func() error { p.Stop(); return nil }
	defer p.Close()
	if err = p.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// boundedBuffer keeps a prefix and reports the full length, so a caller can see
// that output was cut rather than silently receiving a shortened copy.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
	total int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += n
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return n, nil
}
func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

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
	index := map[string]bool{}
	for _, name := range hosted {
		index[name] = true
	}
	var surfaces []requestSurface
	for _, body := range requests {
		if body == nil {
			return &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeUnreadable, Phase: BeforeLaunch}
		}
		surface, ok := readSurface(body, o.Restriction.Tools.Server, index)
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
	if failure := judgeSurfaces(string(o.Engine), o.Restriction.Tools.Server, hosted, surfaces, l.host.served()); failure != nil {
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
	prompt, _ := json.Marshal(map[string]any{"type": "user", "session_id": id, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": "Capability check only."}})
	cmd := exec.CommandContext(ctx, o.Binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(append(prompt, '\n'))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	p, err := process.New(cmd)
	if err != nil {
		return err
	}
	cmd.Cancel = func() error { p.Stop(); return nil }
	defer p.Close()
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
	if _, err = w.request(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "lib-agent-harness", "version": "1"}, "capabilities": map[string]any{}}); err != nil {
		return err
	}
	if err = w.send(ctx, map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	params := map[string]any{"cwd": dir, "approvalPolicy": o.Policy.CodexApproval, "sandbox": o.Policy.CodexSandbox, "model": o.Model}
	if o.Instructions.Mode == Append {
		params["developerInstructions"] = o.Instructions.Text
	}
	body, err := w.request(ctx, "thread/start", params)
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
	turn := map[string]any{"threadId": started.Thread.ID, "input": []any{map[string]any{"type": "text", "text": "Capability check only."}}}
	if o.Effort != "" {
		turn["effort"] = o.Effort
	}
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
