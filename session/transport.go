package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
	"github.com/shhac/lib-agent-harness/process"
)

// wire is intentionally private: callers configure native harnesses; protocol
// tests substitute a transport without executing models or requiring logins.
type wire interface {
	send(context.Context, map[string]any) error
	request(context.Context, string, map[string]any) (json.RawMessage, error)
	close()
}
type response struct {
	body json.RawMessage
	err  error
}
type streamWire struct {
	engine    harness.Engine
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stop      func()
	writeGate chan struct{}
	mu        sync.Mutex
	pending   map[string]chan response
	failure   error
	done      chan struct{}
	reaped    chan struct{}
	once      sync.Once
	next      atomic.Uint64
	event     func(map[string]json.RawMessage)
	ended     func(error)
	stderr    *boundedBuffer
	exit      *ProcessError
	diagnose  func(Diagnostic)
	// grokPermission is how this Grok session answers permission requests.
	grokPermission string
	// commandCodePermission is how this Command Code session answers
	// permission requests.
	commandCodePermission string
}

// asyncWire sends a request now and delivers its reply later, for a request
// whose reply is the end of a long operation, such as a Grok prompt.
type asyncWire interface {
	call(context.Context, string, map[string]any) (<-chan response, func(), error)
}

func newProcessWire(ctx context.Context, o Options, nativeID string, resuming bool, l *launch, onStart func(int), event func(map[string]json.RawMessage), ended func(error)) (*streamWire, error) {
	return newProcessWireArgs(ctx, o, commandArgs(o, nativeID, resuming, l), nil, onStart, event, ended)
}

// newProcessWireArgs launches a contained harness. env overrides the session's
// own environment; a capability probe uses that to run with a disposable home
// and a dummy credential instead of the caller's login.
func newProcessWireArgs(ctx context.Context, o Options, args, env []string, onStart func(int), event func(map[string]json.RawMessage), ended func(error)) (*streamWire, error) {
	runCtx, cancel := context.WithCancel(ctx)
	cmd, p, err := process.Command(runCtx, o.Provider.CLI.Binary, args...)
	if err != nil {
		cancel()
		return nil, ErrTransport
	}
	if o.Background {
		p.Background()
	}
	cmd.Dir = o.WorkDir
	if env == nil {
		env = environment(o)
	}
	cmd.Env = env
	// A harness's own standard error is the only local evidence of why a startup
	// or a stream ended. Keep a bounded tail for the caller's diagnostic hook;
	// it never enters an error value, for the same reason provider text does not.
	stderr := &boundedBuffer{limit: 8 << 10}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		p.Close()
		cancel()
		return nil, ErrTransport
	}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	if onStart != nil {
		p.Notify(onStart)
	}
	w := &streamWire{engine: o.Provider.Engine, stdin: stdin, stdout: reader, pending: map[string]chan response{}, done: make(chan struct{}), reaped: make(chan struct{}), writeGate: make(chan struct{}, 1), event: event, ended: ended, stderr: stderr, diagnose: o.OnDiagnostic, grokPermission: o.Policy.GrokPermission, commandCodePermission: o.Policy.CommandCodePermission}
	w.stop = func() { cancel(); p.Stop(); stdin.Close(); reader.Close() }
	go w.read()
	go func() {
		defer close(w.reaped)
		err := p.Run()
		p.Close()
		w.observeExit(err)
		writer.CloseWithError(err)
		cancel()
	}()
	return w, nil
}

// observeExit records how the harness ended. A stream that stops because its
// process died is a different fact from a stream that stopped on its own, and
// collapsing both into one transport error is what made every failure unknown.
func (w *streamWire) observeExit(err error) {
	exit := &ProcessError{Engine: w.engine, Code: ProcessExited}
	var status *exec.ExitError
	switch {
	case err == nil:
		exit.Code = ProcessExited
	case errors.As(err, &status):
		code := status.ExitCode()
		exit.ExitCode = &code
		if code < 0 {
			exit.Code = ProcessSignalled
		}
	default:
		exit.Code = ProcessStartFailed
	}
	w.mu.Lock()
	w.exit = exit
	detail := sanitize(w.stderr.Bytes(), 2048)
	report := w.diagnose
	w.mu.Unlock()
	if report != nil {
		report(Diagnostic{Engine: w.engine, Stage: "process_exit", Code: exit.Code, Detail: detail, At: time.Now().UTC()})
	}
}

// processError reports the recorded exit when one is known, so a transport
// failure caused by a dead harness says so.
func (w *streamWire) processError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exit == nil {
		return nil
	}
	return w.exit
}
func (w *streamWire) close() { w.once.Do(func() { close(w.done); w.stop() }) }
func (w *streamWire) fail(err error) {
	w.mu.Lock()
	if w.failure == nil {
		w.failure = err
	}
	w.mu.Unlock()
	w.ended(err)
	w.close()
}
func (w *streamWire) terminalError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failure != nil {
		return w.failure
	}
	return ErrClosed
}

// endOfStream distinguishes a harness that died from one whose stream simply
// ended. The process is reaped concurrently, so wait briefly for its status
// rather than reporting a generic transport failure that was actually an exit.
func (w *streamWire) endOfStream() error {
	select {
	case <-w.reaped:
	case <-time.After(2 * time.Second):
	}
	if exit := w.processError(); exit != nil {
		return exit
	}
	return ErrTransport
}
func (w *streamWire) send(ctx context.Context, msg map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case w.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return w.terminalError()
	}
	defer func() { <-w.writeGate }()
	select {
	case <-w.done:
		return w.terminalError()
	default:
	}
	// Closing the session on a cancelled write avoids an ambiguous late prompt.
	completed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-completed:
				return
			default:
			}
			w.close()
		case <-completed:
		case <-w.done:
		}
	}()
	err := json.NewEncoder(w.stdin).Encode(msg)
	close(completed)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		select {
		case <-w.done:
			return w.terminalError()
		default:
			return ErrTransport
		}
	}
	return nil
}
func (w *streamWire) request(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	ch, forget, err := w.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	defer forget()
	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, w.terminalError()
	}
}

// call sends a request and returns where its reply will arrive. forget stops
// listening for it.
func (w *streamWire) call(ctx context.Context, method string, params map[string]any) (<-chan response, func(), error) {
	id := strconv.FormatUint(w.next.Add(1), 10)
	ch := make(chan response, 1)
	w.mu.Lock()
	w.pending[id] = ch
	w.mu.Unlock()
	forget := func() { w.mu.Lock(); delete(w.pending, id); w.mu.Unlock() }
	if err := w.send(ctx, w.envelope(id, method, params)); err != nil {
		forget()
		return nil, nil, err
	}
	return ch, forget, nil
}

// envelope frames a request in the engine's dialect.
func (w *streamWire) envelope(id, method string, params map[string]any) map[string]any {
	switch w.engine {
	case harness.Claude:
		params = cloneMap(params)
		params["subtype"] = method
		return map[string]any{"type": "control_request", "request_id": id, "request": params}
	case harness.Grok, harness.CommandCode:
		return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	}
	return map[string]any{"id": id, "method": method, "params": params}
}

func cloneMap(m map[string]any) map[string]any {
	n := make(map[string]any, len(m)+1)
	for k, v := range m {
		n[k] = v
	}
	return n
}
func (w *streamWire) read() {
	scanner := bufio.NewScanner(w.stdout)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil || m == nil {
			w.fail(ErrProtocol)
			return
		}
		if w.reply(m) {
			continue
		}
		if w.serverRequest(m) {
			continue
		}
		w.event(m)
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		w.fail(ErrOutputLimit)
	} else {
		w.fail(w.endOfStream())
	}
}
func (w *streamWire) reply(m map[string]json.RawMessage) bool {
	parse := parseCodexReply
	switch w.engine {
	case harness.Claude:
		parse = parseClaudeReply
	case harness.Grok:
		parse = parseGrokReply
	case harness.CommandCode:
		parse = parseCommandCodeReply
	}
	id, r, isReply, err := parse(m)
	if !isReply {
		return false
	}
	if err != nil {
		w.fail(err)
		return true
	}
	w.mu.Lock()
	ch := w.pending[id]
	w.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
	return true
}

// parseClaudeReply reads a control_response into the request it answers and
// its outcome. isReply is false for any other frame, and a reply that cannot be
// read is ErrProtocol.
func parseClaudeReply(m map[string]json.RawMessage) (id string, r response, isReply bool, err error) {
	if str(m, "type") != "control_response" {
		return "", response{}, false, nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(m["response"], &data) != nil || data == nil {
		return "", response{}, true, ErrProtocol
	}
	id = str(data, "request_id")
	if id == "" {
		return "", response{}, true, ErrProtocol
	}
	switch str(data, "subtype") {
	case "success":
		body := data["response"]
		if !rawjson.Absent(body) {
			var payload map[string]json.RawMessage
			if json.Unmarshal(body, &payload) != nil {
				return "", response{}, true, ErrProtocol
			}
		}
		return id, response{body: body}, true, nil
	case "error":
		var message string
		if json.Unmarshal(data["error"], &message) != nil {
			return "", response{}, true, ErrProtocol
		}
		if strings.HasPrefix(message, "Unsupported control request subtype:") {
			return id, response{err: ErrUnsupported}, true, nil
		}
		return id, response{err: ErrRejected}, true, nil
	default:
		return "", response{}, true, ErrProtocol
	}
}

// parseCodexReply reads a JSON-RPC response into the request it answers and its
// outcome. isReply is false for a notification or a server request, and a reply
// that cannot be read is ErrProtocol.
func parseCodexReply(m map[string]json.RawMessage) (id string, r response, isReply bool, err error) {
	if len(m["method"]) != 0 || len(m["id"]) == 0 {
		return "", response{}, false, nil
	}
	id = str(m, "id")
	if id == "" {
		return "", response{}, true, ErrProtocol
	}
	if rawjson.Absent(m["error"]) {
		if len(m["result"]) == 0 {
			return "", response{}, true, ErrProtocol
		}
		return id, response{body: m["result"]}, true, nil
	}
	var e struct{ Code *int }
	if json.Unmarshal(m["error"], &e) != nil || e.Code == nil || len(m["result"]) != 0 {
		return "", response{}, true, ErrProtocol
	}
	if *e.Code == -32601 {
		return id, response{err: ErrUnsupported}, true, nil
	}
	return id, response{err: ErrRejected}, true, nil
}

// All unexpected server-originated operations fail closed. We never grant a
// permission just to unblock a native harness. Payloads are not exposed as errors.
func (w *streamWire) serverRequest(m map[string]json.RawMessage) bool {
	var reply map[string]any
	switch w.engine {
	case harness.Claude:
		if str(m, "type") != "control_request" {
			return false
		}
		reply = map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": str(m, "request_id"), "error": "Client does not authorize this operation"}}
	case harness.Grok:
		if len(m["id"]) == 0 || len(m["method"]) == 0 {
			return false
		}
		reply = grokServerReply(m, w.grokPermission)
	case harness.CommandCode:
		if len(m["id"]) == 0 || len(m["method"]) == 0 {
			return false
		}
		reply = commandCodeServerReply(m, w.commandCodePermission)
	default:
		if len(m["id"]) == 0 || len(m["method"]) == 0 {
			return false
		}
		reply = map[string]any{"id": m["id"], "error": map[string]any{"code": -32601, "message": "Client does not authorize this operation"}}
		switch str(m, "method") {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			delete(reply, "error")
			reply["result"] = map[string]any{"decision": "decline"}
		case "item/permissions/requestApproval":
			delete(reply, "error")
			reply["result"] = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		case "mcpServer/elicitation/request":
			delete(reply, "error")
			reply["result"] = map[string]any{"action": "decline", "content": nil}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.send(ctx, reply); err != nil && !errors.Is(err, ErrClosed) {
		w.fail(ErrTransport)
	}
	return true
}
func str(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

func flag(m map[string]json.RawMessage, k string) bool {
	var b bool
	_ = json.Unmarshal(m[k], &b)
	return b
}

func runOnce(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
	cmd, p, err := process.Command(ctx, binary, args...)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	cmd.Dir = dir
	cmd.Env = env
	out := &boundedBuffer{limit: 4 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
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
