package session

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// privateDir mirrors what an application supplies: a directory only its owner
// can enter. Go's own temporary directories are group- and world-executable,
// which the tool host deliberately refuses.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testHost(t *testing.T, handler ToolHandler, tools ...ToolDefinition) *toolHost {
	t.Helper()
	if len(tools) == 0 {
		tools = []ToolDefinition{{Name: "read_file", Description: "read", Schema: map[string]any{"type": "object"}}}
	}
	h, err := newToolHost(ToolHost{Server: "workspace", Tools: tools, Handler: handler, Dir: privateDir(t), Bridge: Bridge{Path: "/usr/bin/true", Args: []string{"tool-bridge"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	return h
}

// client speaks the protocol a bridge relays, so tests exercise the real frames
// rather than calling the dispatcher directly.
type client struct {
	conn net.Conn
	read *bufio.Scanner
	mu   sync.Mutex
	next int
}

func dial(t *testing.T, h *toolHost, secret string) *client {
	t.Helper()
	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	hello, _ := json.Marshal(map[string]string{"secret": secret})
	if _, err = conn.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxToolRequestBytes)
	return &client{conn: conn, read: scanner}
}

func (c *client) post(method string, params any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	_, err := c.conn.Write(append(raw, '\n'))
	return err
}

func (c *client) send(t *testing.T, method string, params any) {
	t.Helper()
	if err := c.post(method, params); err != nil {
		t.Fatal(err)
	}
}

func (c *client) receive(t *testing.T) map[string]any {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if !c.read.Scan() {
		t.Fatalf("no reply: %v", c.read.Err())
	}
	var frame map[string]any
	if err := json.Unmarshal(c.read.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func (c *client) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	c.send(t, "tools/call", map[string]any{"name": name, "arguments": args})
	frame := c.receive(t)
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", frame)
	}
	content, _ := result["content"].([]any)
	text := ""
	if len(content) > 0 {
		block, _ := content[0].(map[string]any)
		text, _ = block["text"].(string)
	}
	isError, _ := result["isError"].(bool)
	return text, isError
}

func echoHandler(t *testing.T) ToolHandler {
	t.Helper()
	return ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{Content: c.Name + ":" + string(c.Arguments)}, nil
	})
}

func TestToolHostServesTheConfiguredSurface(t *testing.T) {
	h := testHost(t, echoHandler(t))
	c := dial(t, h, string(h.secret))
	c.send(t, "initialize", map[string]any{"protocolVersion": "2024-11-05"})
	frame := c.receive(t)
	result := frame["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("did not negotiate the harness's version: %v", result)
	}
	if result["serverInfo"].(map[string]any)["name"] != "workspace" {
		t.Errorf("wrong server identity: %v", result)
	}
	c.send(t, "tools/list", map[string]any{})
	tools := c.receive(t)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "read_file" {
		t.Fatalf("unexpected tool list: %v", tools)
	}
	if text, isError := c.call(t, "read_file", map[string]any{"path": "a.go"}); isError || !strings.Contains(text, `"path":"a.go"`) {
		t.Fatalf("call did not reach the handler: %q %v", text, isError)
	}
}

// Filesystem permissions protect the channel, and the credential proves the
// connection came from the bridge this session started.
func TestToolHostRefusesAnUnauthenticatedConnection(t *testing.T) {
	called := false
	h := testHost(t, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		called = true
		return ToolResult{}, nil
	}))
	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.Write([]byte("{\"secret\":\"wrong\"}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if n, _ := conn.Read(make([]byte, 64)); n != 0 {
		t.Error("an unauthenticated connection received a reply")
	}
	if called {
		t.Error("an unauthenticated connection reached the handler")
	}
	if _, err = os.Stat(filepath.Join(h.cfg.Dir, "t.secret")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(h.cfg.Dir, "t.secret"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("channel credential is not owner-only: %v", info.Mode())
	}
}

func TestToolHostRefusesUnknownAndMalformedCalls(t *testing.T) {
	h := testHost(t, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Error("handler ran for a call that should have been refused")
		return ToolResult{}, nil
	}))
	c := dial(t, h, string(h.secret))
	if text, isError := c.call(t, "run_command", map[string]any{}); !isError || !strings.Contains(text, "nothing was executed") {
		t.Errorf("unknown tool was not refused: %q", text)
	}
	c.send(t, "tools/call", map[string]any{"name": "read_file", "arguments": "not an object"})
	result := c.receive(t)["result"].(map[string]any)
	if isError, _ := result["isError"].(bool); !isError {
		t.Errorf("malformed arguments were not refused: %v", result)
	}
	c.send(t, "unsupported/method", map[string]any{})
	if _, ok := c.receive(t)["error"]; !ok {
		t.Error("unsupported method did not produce an error reply")
	}
}

// A closing tool has to shut the channel the moment it is admitted. Native
// turns issue several calls at once, so latching on return would let work
// continue after the session has already reported that it finished.
func TestClosingToolRefusesLaterCallsImmediately(t *testing.T) {
	running := make(chan struct{})
	release := make(chan struct{})
	executed := make(chan string, 8)
	h := testHost(t,
		ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
			if c.Name == "finish" {
				close(running)
				<-release
				return ToolResult{Content: "reported"}, nil
			}
			executed <- c.Name
			return ToolResult{Content: "ran"}, nil
		}),
		ToolDefinition{Name: "finish", Schema: map[string]any{"type": "object"}, Closing: true},
		ToolDefinition{Name: "write_file", Schema: map[string]any{"type": "object"}},
	)
	refusals := make(chan string, 8)
	h.onRefusal = func(tool, reason string) { refusals <- tool + ":" + reason }
	finisher := dial(t, h, string(h.secret))
	go func() {
		_ = finisher.post("tools/call", map[string]any{"name": "finish", "arguments": map[string]any{}})
	}()
	<-running

	// Arrives while the closing tool is still executing: it must not run.
	parallel := dial(t, h, string(h.secret))
	text, isError := parallel.call(t, "write_file", map[string]any{"path": "x"})
	if !isError || !strings.Contains(text, "nothing was executed") {
		t.Fatalf("a parallel call ran after the channel closed: %q %v", text, isError)
	}
	close(release)
	if frame := finisher.receive(t); frame["result"] == nil {
		t.Fatalf("closing tool did not complete: %v", frame)
	}
	if _, isError = parallel.call(t, "write_file", map[string]any{"path": "y"}); !isError {
		t.Error("a later call ran after the channel closed")
	}
	select {
	case name := <-executed:
		t.Fatalf("tool %q executed after the channel closed", name)
	default:
	}
	if got := <-refusals; !strings.HasSuffix(got, ":channel_closed") {
		t.Errorf("refusal was not reported as a closed channel: %q", got)
	}
	if !h.channelClosed() {
		t.Error("channel did not latch shut")
	}
}

// Admitting a closing tool cancels work already running. An effect cannot be
// undone, but it can be stopped from continuing.
func TestClosingToolCancelsConcurrentWork(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	h := testHost(t,
		ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
			if c.Name == "run_command" {
				close(started)
				<-ctx.Done()
				close(cancelled)
				return ToolResult{}, ctx.Err()
			}
			return ToolResult{Content: "reported"}, nil
		}),
		ToolDefinition{Name: "run_command", Schema: map[string]any{"type": "object"}},
		ToolDefinition{Name: "ask_decision", Schema: map[string]any{"type": "object"}, Closing: true},
	)
	slow := dial(t, h, string(h.secret))
	go func() {
		_ = slow.post("tools/call", map[string]any{"name": "run_command", "arguments": map[string]any{}})
	}()
	<-started
	closer := dial(t, h, string(h.secret))
	if _, isError := closer.call(t, "ask_decision", map[string]any{}); isError {
		t.Fatal("closing tool was refused")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent work was not cancelled")
	}
	text, isError := "", false
	result := slow.receive(t)["result"].(map[string]any)
	if content, _ := result["content"].([]any); len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	isError, _ = result["isError"].(bool)
	if !isError || !strings.Contains(text, "effect is unknown") {
		t.Errorf("cancelled work did not report uncertainty: %q", text)
	}
}

func TestToolResultsAreBoundedWithAnExplicitMarker(t *testing.T) {
	h := testHost(t, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Content: strings.Repeat("x", 200000)}, nil
	}))
	c := dial(t, h, string(h.secret))
	text, _ := c.call(t, "read_file", map[string]any{})
	if len(text) > 70000 || !strings.Contains(text, "truncated") {
		t.Fatalf("result was not bounded with a marker: %d bytes", len(text))
	}
}

// A probe must not be able to execute anything, whatever the harness does.
func TestProbingHostRefusesEveryCall(t *testing.T) {
	h := testHost(t, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Error("a tool ran during a capability check")
		return ToolResult{}, nil
	}))
	h.setProbing(true)
	c := dial(t, h, string(h.secret))
	if text, isError := c.call(t, "read_file", map[string]any{}); !isError || !strings.Contains(text, "capability check") {
		t.Fatalf("probe did not refuse the call: %q", text)
	}
}

func TestToolHostValidationRejectsUnsafeConfiguration(t *testing.T) {
	good := ToolHost{Server: "workspace", Tools: []ToolDefinition{{Name: "read_file", Schema: map[string]any{}}}, Handler: echoHandler(t), Dir: privateDir(t), Bridge: Bridge{Path: "/usr/bin/true"}}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ToolHost){
		"no server":      func(h *ToolHost) { h.Server = "" },
		"bad server":     func(h *ToolHost) { h.Server = "work space" },
		"no handler":     func(h *ToolHost) { h.Handler = nil },
		"no tools":       func(h *ToolHost) { h.Tools = nil },
		"duplicate tool": func(h *ToolHost) { h.Tools = append(h.Tools, h.Tools[0]) },
		"no schema":      func(h *ToolHost) { h.Tools = []ToolDefinition{{Name: "read_file"}} },
		"relative bridge": func(h *ToolHost) {
			h.Bridge.Path = "agent-assistant"
		},
		"world readable dir": func(h *ToolHost) { h.Dir = shared },
		"missing dir":        func(h *ToolHost) { h.Dir = filepath.Join(shared, "absent") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			cfg.Tools = append([]ToolDefinition(nil), good.Tools...)
			mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("unsafe tool host configuration was accepted")
			}
		})
	}
}

func TestQualifiedNamesIdentifyHostedTools(t *testing.T) {
	h := ToolHost{Server: "workspace", Tools: []ToolDefinition{{Name: "read_file"}, {Name: "finish"}}}
	got := strings.Join(h.Qualified(), " ")
	if got != "mcp__workspace__read_file mcp__workspace__finish" {
		t.Fatalf("unexpected identifiers: %s", got)
	}
}
