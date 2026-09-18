package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Tool hosting lets a caller give a native session a tool surface it implements
// itself. The library serves the protocol and owns the channel's lifetime; it
// never executes a tool's effect, and it never reads a file, opens a socket or
// starts a container on a tool's behalf.

// Environment variables naming the private tool channel for a bridge process.
// They carry paths, never secret bytes: the credential lives in an owner-only
// file so it cannot be read out of a process listing.
const (
	BridgeSocketEnv = "AGENT_HARNESS_TOOL_SOCKET"
	BridgeSecretEnv = "AGENT_HARNESS_TOOL_SECRET_FILE"
	BridgeLockEnv   = "AGENT_HARNESS_TOOL_LOCK_FILE"
)

// MaxToolRequestBytes bounds one protocol frame arriving from the harness.
const MaxToolRequestBytes = 1 << 20

var (
	// ErrToolsClosed reports a tool call made after the channel was closed by a
	// closing tool. Nothing was executed.
	ErrToolsClosed = errors.New("harness tool channel is closed")
	// ErrToolUnknown reports a call naming a tool this session does not host.
	ErrToolUnknown = errors.New("harness tool is not hosted by this session")
)

// ToolDefinition describes one caller-implemented tool. Schema must be a JSON
// Schema object describing the tool's arguments.
//
// Closing marks a tool that ends the session's work — reporting completion,
// asking for a decision, handing off to another party. Because calls execute
// one at a time, a closing tool runs with nothing else in flight, and the
// channel latches shut only if the call actually succeeded: a rejected finish
// has not finished anything.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
	Closing     bool           `json:"closing,omitempty"`
}

// ToolCall is one harness request for a caller-implemented tool. Arguments is
// the raw JSON object the model supplied and is untrusted input.
type ToolCall struct {
	TurnID    string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is what the model sees. Content is truncated to the host's limit
// with an explicit marker. Closes latches the channel shut for a tool whose
// closing behaviour depends on its arguments rather than its name. Neither
// Closes nor a Closing definition latches when IsError is set: a refusal is not
// a completion.
type ToolResult struct {
	Content string
	IsError bool
	Closes  bool
}

type ToolHandler interface {
	CallTool(context.Context, ToolCall) (ToolResult, error)
}
type ToolHandlerFunc func(context.Context, ToolCall) (ToolResult, error)

func (f ToolHandlerFunc) CallTool(ctx context.Context, c ToolCall) (ToolResult, error) {
	return f(ctx, c)
}

// Bridge is the caller's own command, re-executed by the harness as its tool
// server. Its only job is to relay the harness's protocol stream to this
// library's listener; session.RunBridge implements exactly that. The library
// never searches PATH for one and never substitutes a shell.
type Bridge struct {
	Path string
	Args []string
}

// ToolHost configures the tool channel. Dir must be an existing owner-only
// directory in private application state: it holds the listener, the channel
// credential and the bridge lock, and it must not be inside anything a tool can
// reach.
type ToolHost struct {
	Server         string
	Tools          []ToolDefinition
	Handler        ToolHandler
	Dir            string
	Bridge         Bridge
	MaxResultBytes int
}

// Qualified returns the identifiers a harness exposes these tools under. They
// are what a restricted session's expected tool surface is compared against.
func (h ToolHost) Qualified() []string {
	names := make([]string, 0, len(h.Tools))
	for _, t := range h.Tools {
		names = append(names, "mcp__"+h.Server+"__"+t.Name)
	}
	return names
}

func validToolName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (h ToolHost) validate() error {
	if !validToolName(h.Server) {
		return errors.New("tool host server name must be a short alphanumeric identifier")
	}
	if h.Handler == nil {
		return errors.New("tool host requires a handler")
	}
	if len(h.Tools) == 0 || len(h.Tools) > 64 {
		return errors.New("tool host requires between 1 and 64 tools")
	}
	seen := map[string]bool{}
	for _, t := range h.Tools {
		if !validToolName(t.Name) || seen[t.Name] {
			return errors.New("tool host names must be unique short alphanumeric identifiers")
		}
		if t.Schema == nil {
			return errors.New("tool host requires an argument schema for every tool")
		}
		seen[t.Name] = true
	}
	if h.Bridge.Path == "" || !filepath.IsAbs(h.Bridge.Path) {
		return errors.New("tool host requires an absolute bridge command path")
	}
	if h.Dir == "" || !filepath.IsAbs(h.Dir) {
		return errors.New("tool host requires an absolute private directory")
	}
	info, err := os.Stat(h.Dir)
	if err != nil || !info.IsDir() {
		return errors.New("tool host directory must exist")
	}
	if err = ownerOnly(info); err != nil {
		return err
	}
	return nil
}

// toolHost owns the private listener and the tool channel's lifetime.
//
// Calls execute one at a time. A native turn will happily ask for several at
// once, and running them concurrently would make "after the work was reported"
// an ambiguous claim: a write or a test could still be in flight when a closing
// tool decides the assignment is finished, and cancelling it afterwards does not
// undo it. Serializing costs some parallelism and buys a boundary the caller can
// actually rely on, including for the evidence it collects.
type toolHost struct {
	cfg       ToolHost
	listener  net.Listener
	socket    string
	socketDir string
	secret    []byte
	lease     *os.File
	tools     map[string]ToolDefinition
	// gate serializes execution across every connection.
	gate chan struct{}

	mu      sync.Mutex
	closed  bool
	probing bool
	stopped bool
	running int
	// pending holds every admitted call, queued or executing, keyed by the
	// connection and request that asked for it.
	pending map[string]*hostedCall
	// paused suspends admission without ending the work, which is what an
	// interrupt needs; closed ends it, which is what a closing tool does.
	paused bool
	// generation moves when the channel reopens, so a call queued before a pause
	// cannot run against the work that follows it.
	generation uint64
	// connections numbers harness connections, because request identifiers are
	// only unique within one.
	connections uint64
	// listed records that a harness connected, authenticated and asked for this
	// session's tools. Where a harness defers MCP tools and never puts them in a
	// request, this is the positive evidence that they were actually offered.
	listed bool

	// Wiring supplied by the session that owns this host. Both are nil for a
	// host serving a capability probe, which has no session and no turn.
	onRefusal  func(tool, reason string)
	activeTurn func() string
	done       chan struct{}
	wg         sync.WaitGroup
}

func newToolHost(cfg ToolHost) (*toolHost, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.MaxResultBytes <= 0 {
		cfg.MaxResultBytes = 64 << 10
	}
	// A local socket path has a hard length limit far below what an ordinary
	// application state directory reaches, so the listener gets its own short
	// owner-only directory. The durable files — the channel credential and the
	// bridge lock that outlives a crash — stay in the caller's directory.
	socketDir, err := shortPrivateDir()
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(socketDir, "t.sock")
	h := &toolHost{cfg: cfg, socket: socket, socketDir: socketDir, tools: map[string]ToolDefinition{}, pending: map[string]*hostedCall{}, gate: make(chan struct{}, 1), done: make(chan struct{})}
	// The assignment lease is taken before anything is launched, so a second
	// process cannot drive this assignment during the window before a bridge
	// exists. It is released when the host closes, or by the operating system if
	// this process dies.
	if h.lease, err = holdLease(filepath.Join(cfg.Dir, "session.lease")); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	for _, t := range cfg.Tools {
		h.tools[t.Name] = t
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("tool host credential unavailable")
	}
	h.secret = []byte(hex.EncodeToString(secret[:]))
	if err = writePrivate(filepath.Join(cfg.Dir, "t.secret"), h.secret); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("tool host could not open its private channel")
	}
	if err = os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("tool host could not restrict its private channel")
	}
	h.listener = listener
	h.wg.Add(1)
	go h.accept()
	return h, nil
}

// shortPrivateDir creates an owner-only directory whose path leaves room for a
// local socket name. The system temporary directory is per-user on the
// platforms this runs on, and the directory itself is owner-only regardless.
func shortPrivateDir() (string, error) {
	base := filepath.Join(os.TempDir(), "agent-harness-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", errors.New("tool host could not prepare its private channel directory")
	}
	if err := os.Chmod(base, 0700); err != nil {
		return "", errors.New("tool host could not restrict its private channel directory")
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("tool host private channel directory is not usable")
	}
	if err = ownerOnly(info); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		return "", errors.New("tool host could not prepare its private channel directory")
	}
	if len(dir) > 90 {
		_ = os.RemoveAll(dir)
		return "", errors.New("tool host private channel path is too long for a local socket")
	}
	return dir, nil
}

func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("tool host could not write its private channel credential")
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return errors.New("tool host could not write its private channel credential")
	}
	return f.Close()
}

// environment names the channel for the bridge process. Only paths travel here.
func (h *toolHost) environment() map[string]string {
	return map[string]string{
		BridgeSocketEnv: h.socket,
		BridgeSecretEnv: filepath.Join(h.cfg.Dir, "t.secret"),
		BridgeLockEnv:   filepath.Join(h.cfg.Dir, "bridge.lock"),
	}
}

func (h *toolHost) close() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	lease := h.lease
	h.lease = nil
	h.mu.Unlock()
	close(h.done)
	_ = h.listener.Close()
	h.wg.Wait()
	if lease != nil {
		_ = lease.Close()
	}
	_ = os.RemoveAll(h.socketDir)
}

// setProbing refuses every call for the duration of a capability probe. The
// probe's provider rejects inference, so no model can ask for one; this makes
// that a property of the host rather than an assumption about the provider.
func (h *toolHost) setProbing(on bool) {
	h.mu.Lock()
	h.probing = on
	h.mu.Unlock()
}

// served reports that the tool channel has handed this session's tools to a
// harness. It says the surface exists and was reachable, nothing more.
func (h *toolHost) served() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listed
}

func (h *toolHost) channelClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *toolHost) accept() {
	defer h.wg.Done()
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer conn.Close()
			h.serve(conn)
		}()
	}
}

// serve reads the bridge's authentication line, then the protocol stream. A
// connection that cannot prove it holds the channel credential is dropped
// without a reply.
func (h *toolHost) serve(conn net.Conn) {
	h.mu.Lock()
	h.connections++
	connection := h.connections
	h.mu.Unlock()
	go func() {
		<-h.done
		_ = conn.Close()
	}()
	reader := bufio.NewReaderSize(conn, 4096)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxToolRequestBytes)
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	if !scanner.Scan() {
		return
	}
	var hello struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal(scanner.Bytes(), &hello) != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(hello.Secret), h.secret) != 1 {
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	writeMu := sync.Mutex{}
	reply := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = conn.Write(append(raw, '\n'))
	}
	var pending sync.WaitGroup
	defer pending.Wait()
	for scanner.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			reply(rpcError(nil, -32700, "parse error"))
			return
		}
		if len(frame.ID) == 0 || string(frame.ID) == "null" {
			// A notification needs no reply, but it is not therefore uninteresting:
			// a harness cancels a tool call this way, and ignoring it leaves work
			// running after the turn that asked for it has been interrupted.
			if frame.Method == "notifications/cancelled" {
				h.cancelRequested(connection, frame.Params)
			}
			continue
		}
		switch frame.Method {
		case "initialize":
			reply(rpcResult(frame.ID, h.initialize(frame.Params)))
		case "ping":
			reply(rpcResult(frame.ID, map[string]any{}))
		case "tools/list":
			h.mu.Lock()
			h.listed = true
			h.mu.Unlock()
			reply(rpcResult(frame.ID, h.list()))
		case "tools/call":
			// Dispatched on its own goroutine so the connection keeps reading —
			// execution itself is serialized inside dispatch, not here.
			//
			// The turn is sampled now, when the harness asked, rather than when
			// the handler eventually runs. A call that waited behind another one
			// belongs to the turn that requested it, and labelling it with a later
			// turn would attribute work to the wrong piece of the conversation.
			id, params := frame.ID, frame.Params
			turn := ""
			if h.activeTurn != nil {
				turn = h.activeTurn()
			}
			pending.Add(1)
			go func() {
				defer pending.Done()
				reply(rpcResult(id, h.dispatch(callKey(connection, string(id)), turn, params)))
			}()
		default:
			// Every other method, including the resource methods a harness's own
			// MCP helpers use, is refused. A restricted session hosts tools and
			// nothing else, so those helpers have nothing here to enumerate or
			// read even on a harness that offers them.
			reply(rpcError(frame.ID, -32601, "method not supported"))
		}
	}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}
func rpcError(id json.RawMessage, code int, message string) map[string]any {
	out := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": code, "message": message}}
	if len(id) > 0 {
		out["id"] = id
	}
	return out
}

// initialize echoes a protocol version the harness asked for when it is one we
// recognize, so an older or newer installed CLI still negotiates successfully.
func (h *toolHost) initialize(params json.RawMessage) map[string]any {
	version := "2025-06-18"
	var in struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &in) == nil {
		switch in.ProtocolVersion {
		case "2024-11-05", "2025-03-26", "2025-06-18":
			version = in.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": h.cfg.Server, "version": "1"},
	}
}

func (h *toolHost) list() map[string]any {
	tools := make([]map[string]any, 0, len(h.cfg.Tools))
	for _, t := range h.cfg.Tools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
	}
	return map[string]any{"tools": tools}
}

func toolPayload(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

func (h *toolHost) refuse(tool, reason, text string) map[string]any {
	if h.onRefusal != nil {
		h.onRefusal(tool, reason)
	}
	return toolPayload(text, true)
}

// refuseLocked is refuse from inside the host's own lock. The notification runs
// after the lock is released so a caller's observer cannot deadlock the channel.
func (h *toolHost) refuseLocked(tool, reason, text string) map[string]any {
	if notify := h.onRefusal; notify != nil {
		go notify(tool, reason)
	}
	return toolPayload(text, true)
}

// dispatch admits a call, executes it and decides whether it ended the work.
//
// Execution is serialized: a call waits for the previous one to finish. That is
// what makes "later" mean something. A closing tool then runs to completion like
// any other, and the channel latches only if it actually succeeded — a finish
// whose arguments were rejected has not finished anything, and neither has one
// whose handler refused it. Nothing is cancelled to make room for it, because a
// half-executed write or test is not a state worth reporting evidence about.
func (h *toolHost) dispatch(request, turn string, params json.RawMessage) map[string]any {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &in) != nil {
		return h.refuse("", "malformed", "tool call could not be parsed; nothing was executed")
	}
	definition, hosted := h.tools[in.Name]
	if !hosted {
		return h.refuse(in.Name, "unknown", "tool "+quoteName(in.Name)+" is not available in this session; nothing was executed")
	}
	if len(in.Arguments) == 0 || string(in.Arguments) == "null" {
		in.Arguments = json.RawMessage("{}")
	}
	// Handlers are written against an argument object. A bare string or array
	// that happens to be valid JSON is not one, and passing it through would
	// leave every handler to rediscover that.
	var arguments map[string]json.RawMessage
	if json.Unmarshal(in.Arguments, &arguments) != nil || arguments == nil {
		return h.refuse(in.Name, "malformed", "tool arguments must be a JSON object; nothing was executed")
	}
	// Registered before it waits for anything. A call queued behind another one
	// is still a call this session is on the hook for: it has to be cancellable,
	// it has to count as unsettled, and it must not slip through a barrier that
	// went up while it was waiting.
	call, refusal := h.admit(request, in.Name)
	if refusal != nil {
		return refusal
	}
	defer h.retire(call)
	if refusal = h.acquire(call, in.Name); refusal != nil {
		return refusal
	}
	defer h.release()
	result, err := h.cfg.Handler.CallTool(call.ctx, ToolCall{TurnID: turn, Name: in.Name, Arguments: in.Arguments})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return toolPayload("tool execution was cancelled; its effect is unknown and must be established from evidence", true)
		}
		return toolPayload(bound(err.Error(), 2048), true)
	}
	if !result.IsError && (definition.Closing || result.Closes) {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
	}
	return toolPayload(bound(result.Content, h.cfg.MaxResultBytes), result.IsError)
}

// hostedCall is one admitted call, tracked from the moment it is accepted until
// it stops — including the time it spends queued.
type hostedCall struct {
	key        string
	ctx        context.Context
	cancel     context.CancelFunc
	generation uint64
}

// admit registers a call and decides whether it may proceed at all. It is the
// single barrier: everything that can refuse a call is here, and a call that
// gets past it is one this host will account for.
func (h *toolHost) admit(request, name string) (*hostedCall, map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	if refusal := h.barrierLocked(name); refusal != nil {
		h.mu.Unlock()
		cancel()
		return nil, refusal
	}
	call := &hostedCall{key: request, ctx: ctx, cancel: cancel, generation: h.generation}
	h.pending[request] = call
	h.mu.Unlock()
	// A stopping session ends everything it admitted.
	go func() {
		select {
		case <-h.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return call, nil
}

func (h *toolHost) retire(call *hostedCall) {
	h.mu.Lock()
	if existing, ok := h.pending[call.key]; ok && existing == call {
		delete(h.pending, call.key)
	}
	h.mu.Unlock()
	call.cancel()
}

// acquire waits for the serialization gate, then checks the barrier again. A
// call can be queued for a long time, and the thing it was waiting behind may
// have finished the work, paused the channel or stopped the session.
func (h *toolHost) acquire(call *hostedCall, name string) map[string]any {
	select {
	case h.gate <- struct{}{}:
	case <-call.ctx.Done():
		return h.refuse(name, "cancelled", "this call was withdrawn before it ran; nothing was executed")
	case <-h.done:
		return h.refuse(name, "stopped", "this session has stopped; nothing was executed")
	}
	h.mu.Lock()
	refusal := h.barrierLocked(name)
	if refusal == nil && call.generation != h.generation {
		// The channel was paused and reopened for different work while this call
		// waited. It belongs to the turn that is over.
		refusal = h.refuseLocked(name, "superseded", "this call belongs to work that has already been stopped; nothing was executed")
	}
	if refusal != nil {
		h.mu.Unlock()
		<-h.gate
		return refusal
	}
	h.running++
	h.mu.Unlock()
	return nil
}

func (h *toolHost) release() {
	h.mu.Lock()
	h.running--
	h.mu.Unlock()
	<-h.gate
}

// barrierLocked is every reason a call may not run, in one place.
func (h *toolHost) barrierLocked(name string) map[string]any {
	switch {
	case h.stopped:
		return h.refuseLocked(name, "stopped", "this session has stopped; nothing was executed")
	case h.probing:
		return h.refuseLocked(name, "probing", "tool calls are refused during a capability check; nothing was executed")
	case h.closed:
		return h.refuseLocked(name, "channel_closed", "work has already been reported or handed over; nothing was executed")
	case h.paused:
		return h.refuseLocked(name, "paused", "this worker's tools are paused; nothing was executed")
	}
	return nil
}

// cancelRequested stops a call the harness has withdrawn. Request identifiers
// are per connection, so the key includes the connection that sent it —
// otherwise one harness's identifier would cancel another's call.
func (h *toolHost) cancelRequested(connection uint64, params json.RawMessage) {
	var in struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &in) != nil {
		return
	}
	h.mu.Lock()
	call := h.pending[callKey(connection, string(in.RequestID))]
	h.mu.Unlock()
	if call != nil {
		call.cancel()
	}
}

func callKey(connection uint64, request string) string {
	return strconv.FormatUint(connection, 10) + ":" + request
}

// CancelTools pauses the tool channel and stops everything it has admitted —
// running and queued alike — so the caller can describe the workspace.
//
// Interrupting a native turn does not reach a caller's tools: measured on both
// installed harnesses, a terminal interrupted result arrived while a hosted
// call was still running. Cancelling only what happened to be executing left
// the queue behind it to run afterwards, which is a write arriving after the
// work was reported as stopped. So this closes admission too, and the channel
// stays closed until the caller opens it for new work.
//
// This is not the same as the channel being finished. A closing tool ends the
// work; a pause suspends it, and the assignment can continue afterwards.
func (s *Session) CancelTools() {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host == nil {
		return
	}
	host.pause()
}

func (h *toolHost) pause() {
	h.mu.Lock()
	h.paused = true
	calls := make([]*hostedCall, 0, len(h.pending))
	for _, call := range h.pending {
		calls = append(calls, call)
	}
	h.mu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

// ResumeTools reopens a paused tool channel for new work. The generation moves,
// so anything still queued from before the pause is refused rather than running
// against a turn that never asked for it.
//
// It does not reopen a channel a closing tool finished: that work is over, and
// reopening it would be a different decision than resuming a pause.
func (s *Session) ResumeTools() {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	host.reopen()
}

// reopen takes only the host's own lock, so a caller already holding the
// session's may use it. A nil host is the unrestricted case and does nothing.
func (h *toolHost) reopen() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.stopped {
		return
	}
	h.paused = false
	h.generation++
}

// ToolsSettled reports that this session is not on the hook for any tool call —
// none executing and none queued. After an interrupt a caller waits for this
// before checkpointing: a turn can end while a tool is still writing, and
// evidence collected in between describes a workspace that was still moving.
func (s *Session) ToolsSettled() bool {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host == nil {
		return true
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.running == 0 && len(host.pending) == 0
}

func quoteName(name string) string {
	name = bound(name, 64)
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range name {
		if r < 0x20 || r == '"' || r == '\\' || r == 0x7f {
			out.WriteByte('?')
			continue
		}
		out.WriteRune(r)
	}
	out.WriteByte('"')
	return out.String()
}

// bound truncates on a rune boundary and says so. Silent truncation would let a
// tool result look complete when it is not.
func bound(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + "\n[truncated: " + strconv.Itoa(len(text)-end) + " further bytes omitted]"
}
