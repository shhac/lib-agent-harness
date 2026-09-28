package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	harness "github.com/shhac/lib-agent-harness"
)

// toolEvents waits for a turn and returns its tool events by kind and item.
func toolEvents(t *testing.T, turn *Turn) map[string]Event {
	t.Helper()
	if _, err := turn.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
	out := map[string]Event{}
	for e := range turn.Events() {
		if strings.HasPrefix(e.Kind, "tool_") {
			out[e.Kind+":"+e.ItemID] = e
		}
	}
	return out
}

func sameJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("input is not JSON: %q", got)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	if string(mustMarshal(a)) != string(mustMarshal(b)) {
		t.Fatalf("input\n got %s\nwant %s", got, want)
	}
}

// Frames are ThreadItem shapes as codex-cli 0.156.1 declares them
// (codex app-server generate-ts), wrapped in item/started and item/completed.
func TestCodexToolEventsCarryInputAndOutput(t *testing.T) {
	s, _, turn := startedCodexTurn(t)
	item := func(method, body string) {
		notify(s, `{"method":"`+method+`","params":{"threadId":"session-1","turnId":"turn-1","startedAtMs":1,"completedAtMs":2,"item":`+body+`}}`)
	}
	item("item/started", `{"type":"commandExecution","id":"cmd","pluginId":null,"scriptPath":null,"command":"/bin/zsh -lc 'ls -1'","cwd":"/work","processId":null,"source":"agent","status":"inProgress","commandActions":[{"type":"listFiles","command":"ls -1","path":null}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}`)
	item("item/completed", `{"type":"commandExecution","id":"cmd","pluginId":null,"scriptPath":null,"command":"/bin/zsh -lc 'ls -1'","cwd":"/work","processId":null,"source":"agent","status":"failed","commandActions":[{"type":"listFiles","command":"ls -1","path":null}],"aggregatedOutput":"a.go\nls: b: No such file\n","exitCode":2,"durationMs":14}`)
	item("item/started", `{"type":"fileChange","id":"patch","changes":[{"path":"/work/a.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-a\n+b\n"}],"status":"inProgress"}`)
	item("item/completed", `{"type":"fileChange","id":"patch","changes":[{"path":"/work/a.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-a\n+b\n"}],"status":"completed"}`)
	item("item/started", `{"type":"mcpToolCall","id":"mcp","server":"worker","tool":"report","status":"inProgress","arguments":{"summary":"done"},"appContext":null,"mcpAppUi":null,"pluginId":null,"readOnlyHint":null,"result":null,"error":null,"durationMs":null}`)
	item("item/completed", `{"type":"mcpToolCall","id":"mcp","server":"worker","tool":"report","status":"completed","arguments":{"summary":"done"},"appContext":null,"mcpAppUi":null,"pluginId":null,"readOnlyHint":null,"result":{"content":[{"type":"text","text":"recorded"},{"type":"text","text":"thanks"}],"structuredContent":null,"_meta":null},"error":null,"durationMs":3}`)
	item("item/completed", `{"type":"mcpToolCall","id":"mcp-err","server":"worker","tool":"report","status":"failed","arguments":{},"appContext":null,"mcpAppUi":null,"pluginId":null,"readOnlyHint":null,"result":null,"error":{"message":"handler refused"},"durationMs":3}`)
	item("item/completed", `{"type":"dynamicToolCall","id":"dyn","namespace":null,"tool":"lookup","arguments":{"q":1},"status":"completed","contentItems":[{"type":"inputText","text":"found"},{"type":"inputImage","imageUrl":"data:x"}],"success":true,"durationMs":1}`)
	item("item/completed", `{"type":"webSearch","id":"web","query":"go generics","action":{"type":"search","query":"go generics","queries":null},"results":null}`)
	notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
	events := toolEvents(t, turn)

	started := events["tool_started:cmd"]
	sameJSON(t, started.Input, `{"command":"/bin/zsh -lc 'ls -1'","cwd":"/work"}`)
	if started.Output != "" || started.ExitCode != nil || started.InputTruncated {
		t.Fatalf("started command %+v", started)
	}
	done := events["tool_completed:cmd"]
	sameJSON(t, done.Input, `{"command":"/bin/zsh -lc 'ls -1'","cwd":"/work"}`)
	if done.Output != "a.go\nls: b: No such file\n" || done.ExitCode == nil || *done.ExitCode != 2 || done.Status != "failed" {
		t.Fatalf("completed command %+v", done)
	}
	sameJSON(t, events["tool_started:patch"].Input, `{"changes":[{"path":"/work/a.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-a\n+b\n"}]}`)
	if e := events["tool_completed:mcp"]; e.Tool != "report" || e.Output != "recorded\nthanks" {
		t.Fatalf("mcp result %+v", e)
	}
	sameJSON(t, events["tool_started:mcp"].Input, `{"summary":"done"}`)
	if e := events["tool_completed:mcp-err"]; e.Output != "handler refused" || e.Status != "failed" {
		t.Fatalf("mcp error %+v", e)
	}
	if e := events["tool_completed:dyn"]; e.Output != "found" {
		t.Fatalf("dynamic result %+v", e)
	}
	sameJSON(t, events["tool_completed:dyn"].Input, `{"q":1}`)
	sameJSON(t, events["tool_completed:web"].Input, `{"query":"go generics","action":{"type":"search","query":"go generics","queries":null}}`)
}

// Frames are Claude Code 2.1.283's stream-json: tool_use input is an object,
// and tool_result content is either a string or text blocks.
func TestClaudeToolEventsCarryInputAndOutput(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"a","content":[{"type":"tool_use","id":"bash","name":"Bash","input":{"command":"go test ./...","description":"Run tests"}},{"type":"tool_use","id":"mcp","name":"mcp__worker__report","input":{"summary":"done"}}]}}`)
	notify(s, `{"type":"user","session_id":"session-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"bash","content":"FAIL\tpkg\n","is_error":true},{"type":"tool_result","tool_use_id":"mcp","content":[{"type":"text","text":"recorded"},{"type":"image","source":{}}]}]}}`)
	finishClaude(s, false)
	events := toolEvents(t, turn)
	sameJSON(t, events["tool_started:bash"].Input, `{"command":"go test ./...","description":"Run tests"}`)
	if e := events["tool_completed:bash"]; e.Output != "FAIL\tpkg\n" || e.Status != "failed" {
		t.Fatalf("bash result %+v", e)
	}
	sameJSON(t, events["tool_started:mcp"].Input, `{"summary":"done"}`)
	if e := events["tool_completed:mcp"]; e.Output != "recorded" || e.Status != "completed" {
		t.Fatalf("mcp result %+v", e)
	}
}

// grokFakeTurn starts a Grok turn over a fake transport whose prompt answers
// only once the test has sent its updates.
func grokFakeTurn(t *testing.T) (*Session, *Turn, func()) {
	t.Helper()
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Grok, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir(), Policy: Policy{GrokPermission: GrokDenyWhenAsked}})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	w := &fakeWire{requestFn: func(method string, _ map[string]any) (json.RawMessage, error) {
		if method == "session/prompt" {
			<-release
			return json.RawMessage(`{"stopReason":"end_turn","_meta":{}}`), nil
		}
		return json.RawMessage(`{}`), nil
	}}
	s := &Session{options: o, ref: reference(o, "session-1"), caps: CapabilitiesFor(harness.Grok), transport: w, done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	t.Cleanup(s.Close)
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	return s, turn, func() { close(release) }
}

// Frames follow grok 1.0.41's agent protocol: tool_call carries rawInput, and
// the final tool_call_update carries rawOutput (any JSON) or content blocks.
func TestGrokToolEventsCarryInputAndOutput(t *testing.T) {
	s, turn, answer := grokFakeTurn(t)
	update := func(u string) {
		notify(s, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":`+u+`}}`)
	}
	update(`{"sessionUpdate":"tool_call","toolCallId":"read","title":"Read","kind":"read","status":"in_progress","rawInput":{"path":"README.md"},"content":[],"locations":[],"_meta":{"x.ai/tool":{"name":"read_file"}}}`)
	update(`{"sessionUpdate":"tool_call_update","toolCallId":"read","status":"completed","content":[],"rawOutput":"# Title\n","locations":[]}`)
	update(`{"sessionUpdate":"tool_call","toolCallId":"count","title":"Count","status":"in_progress","rawInput":{"path":"src"},"_meta":{"x.ai/tool":{"name":"count_lines"}}}`)
	update(`{"sessionUpdate":"tool_call_update","toolCallId":"count","status":"completed","rawOutput":{"lines":42}}`)
	update(`{"sessionUpdate":"tool_call","toolCallId":"sh","title":"Shell","status":"pending"}`)
	update(`{"sessionUpdate":"tool_call_update","toolCallId":"sh","status":"in_progress","rawInput":{"command":"make"}}`)
	update(`{"sessionUpdate":"tool_call_update","toolCallId":"sh","status":"failed","rawInput":{"command":"make"},"content":[{"type":"content","content":{"type":"text","text":"make: *** no rule"}}]}`)
	update(`{"sessionUpdate":"tool_call_update","toolCallId":"edit","status":"completed","content":[{"type":"diff","path":"/w/a","oldText":"a","newText":"b"}]}`)
	answer()
	events := toolEvents(t, turn)
	sameJSON(t, events["tool_started:read"].Input, `{"path":"README.md"}`)
	if e := events["tool_completed:read"]; e.Output != "# Title\n" || e.Input != nil {
		t.Fatalf("read result %+v", e)
	}
	if e := events["tool_completed:count"]; e.Output != `{"lines":42}` {
		t.Fatalf("object output %+v", e)
	}
	if e := events["tool_started:sh"]; e.Input != nil {
		t.Fatalf("absent rawInput reported %+v", e)
	}
	done := events["tool_completed:sh"]
	sameJSON(t, done.Input, `{"command":"make"}`)
	if done.Output != "make: *** no rule" || done.Status != "failed" {
		t.Fatalf("failed shell %+v", done)
	}
	if e := events["tool_completed:edit"]; !strings.Contains(e.Output, `"newText":"b"`) {
		t.Fatalf("diff content dropped %+v", e)
	}
}

// An API session reports the model's arguments as it proposed them and the
// result the handler returned, which is what the model is answered with.
func TestAPIToolEventsCarryInputAndOutput(t *testing.T) {
	e := newEndpoint(t,
		answer("", scriptedCall{"call_1", "read_file", `{"path":"a"}`}, scriptedCall{"call_2", "read_file", `{"path":"missing"}`}),
		answer("done"),
	)
	handler := ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if strings.Contains(string(c.Arguments), "missing") {
			return ToolResult{Content: "no such file", IsError: true}, nil
		}
		return ToolResult{Content: "contents of " + string(c.Arguments)}, nil
	})
	s := startAPI(t, apiOptions(t, e.url, handler))
	done := runAPITurnToEnd(t, s, "Read.")
	if done.err != nil {
		t.Fatal(done.err)
	}
	events := map[string]Event{}
	for _, ev := range done.events {
		events[ev.Kind+":"+ev.ItemID] = ev
	}
	sameJSON(t, events["tool_started:call_1"].Input, `{"path":"a"}`)
	if e := events["tool_completed:call_1"]; e.Output != `contents of {"path":"a"}` || e.Status != "completed" || e.Input != nil {
		t.Fatalf("result %+v", e)
	}
	sameJSON(t, events["tool_started:call_2"].Input, `{"path":"missing"}`)
	if e := events["tool_completed:call_2"]; e.Status != "failed" || e.Output != "no such file" {
		t.Fatalf("failed call %+v", e)
	}
}

// Both payloads are bounded per event. A truncated input stays valid JSON, as
// a string holding the head of the original text; a truncated output is cut on
// a character boundary.
func TestToolPayloadsAreBounded(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("é<", MaxToolPayloadBytes)
	notify(s, string(mustMarshal(map[string]any{"type": "assistant", "session_id": "session-1", "message": map[string]any{"id": "a", "content": []any{
		map[string]any{"type": "tool_use", "id": "big", "name": "Write", "input": map[string]any{"content": huge}},
		map[string]any{"type": "tool_use", "id": "small", "name": "Read", "input": map[string]any{"path": "a"}},
	}}})))
	notify(s, string(mustMarshal(map[string]any{"type": "user", "session_id": "session-1", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "big", "content": huge},
		map[string]any{"type": "tool_result", "tool_use_id": "small", "content": "ok"},
	}}})))
	finishClaude(s, false)
	events := toolEvents(t, turn)
	in := events["tool_started:big"]
	var head string
	if !in.InputTruncated || len(in.Input) > MaxToolPayloadBytes || json.Unmarshal(in.Input, &head) != nil || !strings.HasPrefix(head, `{"content":"é`) {
		t.Fatalf("input truncated=%v len=%d head=%.40q", in.InputTruncated, len(in.Input), in.Input)
	}
	out := events["tool_completed:big"]
	if !out.OutputTruncated || len(out.Output) > MaxToolPayloadBytes || len(out.Output) < MaxToolPayloadBytes-3 || !utf8.ValidString(out.Output) || !strings.HasPrefix(huge, out.Output) {
		t.Fatalf("output truncated=%v len=%d", out.OutputTruncated, len(out.Output))
	}
	if e := events["tool_started:small"]; e.InputTruncated || string(e.Input) != `{"path":"a"}` {
		t.Fatalf("small input %+v", e)
	}
	if e := events["tool_completed:small"]; e.OutputTruncated || e.Output != "ok" {
		t.Fatalf("small output %+v", e)
	}
	encoded, err := json.Marshal(in)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("a truncated event does not encode: %v", err)
	}
}

// A shell in a sandboxed session can read the tool channel's credential file.
// Tool payloads are the application's data, but that credential is the
// library's, and it never leaves in an event.
func TestToolPayloadsNeverCarryTheChannelCredential(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	secret := strings.Repeat("ab12", 16)
	s.tools = &toolHost{secret: []byte(secret), done: make(chan struct{})}
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"a","content":[{"type":"tool_use","id":"cat","name":"Bash","input":{"command":"echo `+secret+`"}}]}}`)
	notify(s, `{"type":"user","session_id":"session-1","message":{"content":[{"type":"tool_result","tool_use_id":"cat","content":"`+secret+`\n"}]}}`)
	finishClaude(s, false)
	events := toolEvents(t, turn)
	for _, e := range events {
		if strings.Contains(string(e.Input), secret) || strings.Contains(e.Output, secret) {
			t.Fatalf("credential in event %+v", e)
		}
	}
	sameJSON(t, events["tool_started:cat"].Input, `{"command":"echo [redacted]"}`)
	if events["tool_completed:cat"].Output != "[redacted]\n" {
		t.Fatalf("%+v", events["tool_completed:cat"])
	}
}

func TestBoundToolInputEdges(t *testing.T) {
	if raw, cut := boundToolInput([]byte(`{"a":1}`), 7); cut || string(raw) != `{"a":1}` {
		t.Fatalf("%s %v", raw, cut)
	}
	if raw, cut := boundToolInput([]byte(`{"a":12}`), 7); !cut || len(raw) > 7 || !json.Valid(raw) {
		t.Fatalf("%s %v", raw, cut)
	}
	if raw, cut := boundToolInput([]byte("\x00\x00\x00\x00"), 8); !cut || len(raw) > 8 || !json.Valid(raw) {
		t.Fatalf("escaping overran the bound: %s %v", raw, cut)
	}
}
