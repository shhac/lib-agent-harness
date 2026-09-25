//go:build !windows

package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// harness stands in for an installed CLI's MCP client: it speaks the protocol
// down a bridge's stdio exactly as the real one does. Driving the framing from
// this side, rather than calling the host's dispatcher, is what makes these
// tests say anything about the path a real harness takes.
type harness struct {
	in     *io.PipeWriter
	out    *bufio.Scanner
	cancel context.CancelFunc
	done   chan error
}

func startHarness(t *testing.T, h *toolHost) *harness {
	t.Helper()
	for key, value := range h.environment() {
		t.Setenv(key, value)
	}
	bridgeIn, harnessWrites := io.Pipe()
	harnessReads, bridgeOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunBridge(ctx, bridgeIn, bridgeOut) }()
	scanner := bufio.NewScanner(harnessReads)
	scanner.Buffer(make([]byte, 4096), MaxToolRequestBytes)
	out := &harness{in: harnessWrites, out: scanner, cancel: cancel, done: done}
	t.Cleanup(out.stop)
	return out
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
	}
}

func (h *harness) request(t *testing.T, id int, method string, params any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.in.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if !h.out.Scan() {
		t.Fatalf("%s produced no reply: %v", method, h.out.Err())
	}
	var frame map[string]any
	if err = json.Unmarshal(h.out.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if frame["jsonrpc"] != "2.0" {
		t.Errorf("reply is not JSON-RPC 2.0: %v", frame)
	}
	// Correlation matters: a harness matches replies to requests by identifier,
	// and a server that echoes the wrong one corrupts the whole conversation.
	if got, ok := frame["id"].(float64); !ok || int(got) != id {
		t.Fatalf("reply carried id %v, want %d", frame["id"], id)
	}
	return frame
}

func (h *harness) notify(t *testing.T, method string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if _, err := h.in.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

// The full negotiation a harness performs, through the bridge, in order.
func TestHarnessProtocolThroughTheBridge(t *testing.T) {
	seen := make(chan string, 4)
	host := testHost(t,
		ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
			seen <- c.Name + " " + string(c.Arguments)
			return ToolResult{Content: "ok"}, nil
		}),
		ToolDefinition{Name: "read_file", Description: "read a file", Schema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
		ToolDefinition{Name: "finish", Description: "report", Schema: map[string]any{"type": "object"}, Closing: true},
	)
	cli := startHarness(t, host)

	result := cli.request(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "test-harness", "version": "1"}})["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("version was not negotiated: %v", result)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("server did not advertise tools: %v", result)
	}
	// A notification carries no identifier and must produce no reply; answering
	// one would desynchronize every later correlation.
	cli.notify(t, "notifications/initialized")

	listed := cli.request(t, 2, "tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
	if len(listed) != 2 {
		t.Fatalf("expected both hosted tools: %v", listed)
	}
	first := listed[0].(map[string]any)
	if first["name"] != "read_file" || first["inputSchema"] == nil {
		t.Errorf("tool was advertised without a usable schema: %v", first)
	}

	called := cli.request(t, 3, "tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{"path": "main.go"}})["result"].(map[string]any)
	if isError, _ := called["isError"].(bool); isError {
		t.Fatalf("call failed: %v", called)
	}
	select {
	case got := <-seen:
		if !strings.Contains(got, `"path":"main.go"`) {
			t.Errorf("handler received %q", got)
		}
	default:
		t.Fatal("call never reached the handler")
	}

	if _, ok := cli.request(t, 4, "resources/list", map[string]any{})["error"]; !ok {
		t.Error("an unsupported method was not refused")
	}
	if _, ok := cli.request(t, 5, "ping", map[string]any{})["result"]; !ok {
		t.Error("ping was not answered")
	}
}

// A harness may restart its tool server. The session outlives that: the new
// bridge authenticates, the lock moves to it, and work continues.
func TestSessionSurvivesABridgeRestart(t *testing.T) {
	host := testHost(t, echoHandler(t))
	first := startHarness(t, host)
	first.request(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if _, isError := readResult(t, first.request(t, 2, "tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{"path": "a"}})); isError {
		t.Fatal("first bridge could not call a tool")
	}
	held, err := readBridgeLock(lockPath(host.cfg.Dir))
	if err != nil || held == nil {
		t.Fatalf("first bridge did not hold the lock: %v %v", held, err)
	}
	first.stop()
	// The lock has to be released, or a replacement bridge could never start.
	deadline := time.Now().Add(3 * time.Second)
	for {
		held, err = readBridgeLock(lockPath(host.cfg.Dir))
		if err == nil && held == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock was not released after the bridge stopped: %v %v", held, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	second := startHarness(t, host)
	second.request(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	text, isError := readResult(t, second.request(t, 2, "tools/call", map[string]any{"name": "read_file", "arguments": map[string]any{"path": "b"}}))
	if isError || !strings.Contains(text, `"path":"b"`) {
		t.Fatalf("replacement bridge could not call a tool: %q %v", text, isError)
	}
	if held, err = readBridgeLock(lockPath(host.cfg.Dir)); err != nil || held == nil {
		t.Fatalf("replacement bridge did not take the lock: %v %v", held, err)
	}
}

// An application's state directory is deep — deeper than a local socket path may
// be — so the channel must not be placed beneath it.
func TestToolHostWorksBeneathADeepApplicationStateDirectory(t *testing.T) {
	deep := privateDir(t)
	for i := 0; i < 6; i++ {
		deep = filepath.Join(deep, strings.Repeat("state-segment", 2))
	}
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	if len(deep) < 120 {
		t.Fatalf("test directory is only %d bytes; it must exceed a socket path limit", len(deep))
	}
	host, err := newToolHost(ToolHost{
		Server: "workspace", Handler: echoHandler(t), Dir: deep,
		Tools:  []ToolDefinition{{Name: "read_file", Schema: map[string]any{"type": "object"}}},
		Bridge: Bridge{Path: "/usr/bin/true"},
	}, nil)
	if err != nil {
		t.Fatalf("a realistic application state path was rejected: %v", err)
	}
	defer host.close()
	if strings.HasPrefix(host.socket, deep) {
		t.Error("the channel was placed beneath the application's state directory")
	}
	c := dial(t, host, string(host.secret))
	if _, isError := c.call(t, "read_file", map[string]any{}); isError {
		t.Error("the channel did not work from a deep state directory")
	}
	// The durable files stay with the assignment, where recovery looks for them.
	for _, name := range []string{"t.secret", "session.lease"} {
		if _, err = os.Stat(filepath.Join(deep, name)); err != nil {
			t.Errorf("%s was not kept with the assignment: %v", name, err)
		}
	}
}

// Everything above is opt-in. An ordinary session must be launched exactly as it
// was before restricted sessions existed.
func TestUnrestrictedLaunchIsUnchanged(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []Engine{Claude, Codex} {
		o, err := normalize(Options{Engine: engine, Binary: "/usr/bin/true", WorkDir: t.TempDir(), Home: t.TempDir(), Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		l, err := prepareLaunch(ctx, o, nil)
		if l != nil || err != nil {
			t.Fatalf("%s: an unrestricted session prepared a restricted runtime: %v %v", engine, l, err)
		}
		args := strings.Join(commandArgs(o, "id", false, nil), " ")
		for _, absent := range []string{"--mcp-config", "--allowedTools", "--setting-sources", "mcp_servers", "model_catalog_json", "--ignore-user-config"} {
			if strings.Contains(args, absent) {
				t.Errorf("%s: unrestricted launch gained %q: %s", engine, absent, args)
			}
		}
	}
	// And its capability reporting says the surface was never checked, rather
	// than implying it was checked and accepted.
	if got := CapabilitiesFor(Claude).RestrictTools.Availability; got != Unknown && got != "" {
		t.Errorf("an unchecked tool surface reported %q", got)
	}
}

// Verification is not optional, and the only thing that skips repeating it is a
// record of the same binary and the same arguments.
func TestVerificationCacheIsKeyedToTheExactBinaryAndArguments(t *testing.T) {
	o, err := normalize(restrictedOptions(t, Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "harness")
	if err = os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	o.Binary = binary
	host, err := newToolHost(o.Restriction.Tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	l := &launch{host: host, extra: claudeRestrictedArgs(host)}
	key, err := verificationKey(o, l)
	if err != nil {
		t.Fatal(err)
	}
	// One live host per assignment: a second one on the same directory is
	// refused, which is the lease doing its job.
	if _, err = newToolHost(o.Restriction.Tools, nil); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a second host took the same assignment: %v", err)
	}
	// The channel's ephemeral paths change every launch and must not participate.
	next := o.Restriction.Tools
	next.Dir = privateDir(t)
	second, err := newToolHost(next, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	again, err := verificationKey(o, &launch{host: second, extra: claudeRestrictedArgs(second)})
	if err != nil {
		t.Fatal(err)
	}
	if key != again {
		t.Error("a new private channel forced the capability check to be repeated")
	}
	// A changed tool surface is a different question.
	changed := o
	changed.Restriction = &Restriction{Tools: o.Restriction.Tools}
	changed.Restriction.Tools.Tools = o.Restriction.Tools.Tools[:1]
	if other, keyErr := verificationKey(changed, l); keyErr != nil || other == key {
		t.Error("a changed tool surface reused an earlier verification")
	}
	// So is a changed binary.
	if err = os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	upgraded, err := verificationKey(o, l)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded == key {
		t.Error("an upgraded harness reused an earlier verification")
	}
	if _, err = verificationKey(Options{Engine: Claude, Binary: filepath.Join(t.TempDir(), "absent"), Restriction: o.Restriction}, l); err == nil {
		t.Error("a missing binary produced a verification key")
	}
}
