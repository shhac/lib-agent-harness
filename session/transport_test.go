package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// This subprocess is a native-protocol fixture, not a model invocation. Running
// it through the production process transport tests pipes, framing, permission
// denial, and closure on each supported operating system.
func init() {
	if os.Getenv("LIB_HARNESS_SESSION_FIXTURE") != "1" {
		return
	}
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	engine := Codex
	sessionID := "fixture-thread"
	if os.Args[1] == "-p" {
		engine = Claude
		for i, arg := range os.Args {
			if (arg == "--session-id" || arg == "--resume") && i+1 < len(os.Args) {
				sessionID = os.Args[i+1]
			}
		}
	}
	mode := os.Getenv("LIB_HARNESS_SESSION_MODE")
	if mode == "oversized" {
		fmt.Println(strings.Repeat("x", MaxFrameBytes+1))
		os.Exit(1)
	}
	if mode == "bad-json" {
		fmt.Println("not JSON")
		os.Exit(1)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	send := func(m map[string]any) {
		if err := encoder.Encode(m); err != nil {
			os.Exit(2)
		}
	}
	permissionID := "request-permission"
	denied := false
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			os.Exit(2)
		}
		if engine == Codex {
			method := str(m, "method")
			id := m["id"]
			if mode == "malformed-control" && method == "initialize" {
				send(map[string]any{"id": id, "error": map[string]any{"code": "invalid"}})
				continue
			}
			if method == "" && string(id) == `"request-permission"` {
				var result struct{ Decision string }
				_ = json.Unmarshal(m["result"], &result)
				denied = result.Decision == "decline"
				continue
			}
			result := map[string]any{}
			switch method {
			case "initialize":
				send(map[string]any{"id": permissionID, "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "should-not-execute"}})
			case "initialized":
				continue
			case "thread/start", "thread/resume":
				result["thread"] = map[string]any{"id": sessionID}
			case "turn/start":
				if !denied {
					os.Exit(3)
				}
				send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": sessionID, "turnId": "fixture-turn", "itemId": "reply", "delta": "fixture answer"}})
				result["turn"] = map[string]any{"id": "fixture-turn"}
				send(map[string]any{"id": id, "result": result})
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": sessionID, "turn": map[string]any{"id": "fixture-turn", "status": "completed"}}})
				continue
			default:
				os.Exit(3)
			}
			send(map[string]any{"id": id, "result": result})
		} else {
			switch str(m, "type") {
			case "control_request":
				if mode == "malformed-control" {
					send(map[string]any{"type": "control_response", "response": []string{"invalid"}})
					continue
				}
				send(map[string]any{"type": "control_request", "request_id": permissionID, "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "should-not-execute"}}})
				send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": str(m, "request_id"), "response": map[string]any{}}})
			case "control_response":
				var r struct{ Subtype string }
				_ = json.Unmarshal(m["response"], &r)
				denied = r.Subtype == "error"
			case "user":
				if !denied {
					os.Exit(3)
				}
				send(map[string]any{"type": "assistant", "session_id": sessionID, "message": map[string]any{"id": "reply", "content": []any{map[string]any{"type": "text", "text": "fixture answer"}}}})
				send(map[string]any{"type": "result", "subtype": "success", "session_id": sessionID, "is_error": false, "result": "fixture answer", "usage": map[string]any{"input_tokens": 2, "output_tokens": 2}})
			default:
				os.Exit(3)
			}
		}
	}
	os.Exit(0)
}
func TestNativePipeStartResumeAndPermissionDenial(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	for _, engine := range []Engine{Codex, Claude} {
		t.Run(string(engine), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			o := Options{Engine: engine, Binary: os.Args[0], Home: t.TempDir(), WorkDir: t.TempDir()}
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := s.StartTurn(ctx, Input{"hello"})
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			result, err := turn.Wait(ctx)
			if err != nil || result.Text != "fixture answer" {
				t.Fatalf("%+v %v", result, err)
			}
			ref := s.Ref()
			s.Close()
			resumed, err := Resume(ctx, o, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			if resumed.Ref() != ref {
				t.Fatal("resume changed reference")
			}
			turn, err = resumed.StartTurn(ctx, Input{"again"})
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			result, err = turn.Wait(ctx)
			if err != nil || result.Text != "fixture answer" {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}
func TestMalformedProcessOutputStopsStartup(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	t.Setenv("LIB_HARNESS_SESSION_MODE", "bad-json")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{Engine: Codex, Binary: os.Args[0], Home: t.TempDir(), WorkDir: t.TempDir()})
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("invalid transport started")
	}
}
func TestInstructionArgumentsAreNative(t *testing.T) {
	for _, mode := range []InstructionMode{Replace, Append} {
		o := Options{Engine: Claude, Instructions: Instructions{mode, "system instruction"}, Policy: Policy{ClaudePermission: "dontAsk", ClaudeTools: []string{}}}
		args := strings.Join(commandArgs(o, "session", false, nil), "\n")
		flag := "--system-prompt"
		if mode == Append {
			flag = "--append-system-prompt"
		}
		if !strings.Contains(args, flag+"\nsystem instruction") || !strings.Contains(args, "--tools=") || !strings.Contains(args, "--session-id\nsession") {
			t.Fatal(args)
		}
	}
}

func TestMalformedControlFailsPendingImmediately(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	t.Setenv("LIB_HARNESS_SESSION_MODE", "malformed-control")
	for _, e := range []Engine{Codex, Claude} {
		t.Run(string(e), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, err := Start(ctx, Options{Engine: e, Binary: os.Args[0], Home: t.TempDir(), WorkDir: t.TempDir()})
			if s != nil {
				s.Close()
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("wanted protocol failure, got %v", err)
			}
		})
	}
}
func TestOversizedFrameReportsOutputLimit(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	t.Setenv("LIB_HARNESS_SESSION_MODE", "oversized")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, Options{Engine: Codex, Binary: os.Args[0], Home: t.TempDir(), WorkDir: t.TempDir()})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("wanted output limit, got %v", err)
	}
}
func TestProtocolRejectionIsNotMalformed(t *testing.T) {
	for _, e := range []Engine{Codex, Claude} {
		t.Run(string(e), func(t *testing.T) {
			ch := make(chan response, 1)
			failed := false
			w := &streamWire{engine: e, pending: map[string]chan response{"1": ch}, done: make(chan struct{}), stop: func() {}, ended: func(error) { failed = true }}
			raw := `{"id":"1","error":{"code":-32000,"message":"secret error"}}`
			if e == Claude {
				raw = `{"type":"control_response","response":{"subtype":"error","request_id":"1","error":"secret error"}}`
			}
			var m map[string]json.RawMessage
			_ = json.Unmarshal([]byte(raw), &m)
			if !w.reply(m) {
				t.Fatal("unrecognized reply")
			}
			r := <-ch
			if !errors.Is(r.err, ErrRejected) || failed || strings.Contains(r.err.Error(), "secret") {
				t.Fatalf("%v failed=%v", r.err, failed)
			}
		})
	}
}

// Every reply shape each engine can send, read without a live wire: whether a
// frame is a reply at all, which request it answers, and its outcome.
func TestReplyParsersReadEveryShape(t *testing.T) {
	type want struct {
		id      string
		body    string
		err     error
		isReply bool
		invalid bool
	}
	for _, tc := range []struct {
		name  string
		parse func(map[string]json.RawMessage) (string, response, bool, error)
		frame string
		want  want
	}{
		{"claude other frame", parseClaudeReply, `{"type":"stream_event"}`, want{}},
		{"claude success", parseClaudeReply, `{"type":"control_response","response":{"subtype":"success","request_id":"7","response":{"ok":true}}}`, want{id: "7", body: `{"ok":true}`, isReply: true}},
		{"claude success without body", parseClaudeReply, `{"type":"control_response","response":{"subtype":"success","request_id":"7"}}`, want{id: "7", isReply: true}},
		{"claude success with null body", parseClaudeReply, `{"type":"control_response","response":{"subtype":"success","request_id":"7","response":null}}`, want{id: "7", body: "null", isReply: true}},
		{"claude success with non-object body", parseClaudeReply, `{"type":"control_response","response":{"subtype":"success","request_id":"7","response":[1]}}`, want{isReply: true, invalid: true}},
		{"claude rejected", parseClaudeReply, `{"type":"control_response","response":{"subtype":"error","request_id":"7","error":"no"}}`, want{id: "7", err: ErrRejected, isReply: true}},
		{"claude unsupported", parseClaudeReply, `{"type":"control_response","response":{"subtype":"error","request_id":"7","error":"Unsupported control request subtype: x"}}`, want{id: "7", err: ErrUnsupported, isReply: true}},
		{"claude error without text", parseClaudeReply, `{"type":"control_response","response":{"subtype":"error","request_id":"7","error":{}}}`, want{isReply: true, invalid: true}},
		{"claude unknown subtype", parseClaudeReply, `{"type":"control_response","response":{"subtype":"maybe","request_id":"7"}}`, want{isReply: true, invalid: true}},
		{"claude no request id", parseClaudeReply, `{"type":"control_response","response":{"subtype":"success"}}`, want{isReply: true, invalid: true}},
		{"claude no response", parseClaudeReply, `{"type":"control_response"}`, want{isReply: true, invalid: true}},
		{"codex notification", parseCodexReply, `{"method":"turn/started","params":{}}`, want{}},
		{"codex server request", parseCodexReply, `{"id":"3","method":"item/tool/call","params":{}}`, want{}},
		{"codex result", parseCodexReply, `{"id":"3","result":{"thread":{}}}`, want{id: "3", body: `{"thread":{}}`, isReply: true}},
		{"codex null error", parseCodexReply, `{"id":"3","error":null,"result":{}}`, want{id: "3", body: `{}`, isReply: true}},
		{"codex no result", parseCodexReply, `{"id":"3"}`, want{isReply: true, invalid: true}},
		{"codex rejected", parseCodexReply, `{"id":"3","error":{"code":-32600,"message":"bad"}}`, want{id: "3", err: ErrRejected, isReply: true}},
		{"codex unsupported", parseCodexReply, `{"id":"3","error":{"code":-32601,"message":"no such method"}}`, want{id: "3", err: ErrUnsupported, isReply: true}},
		{"codex error without code", parseCodexReply, `{"id":"3","error":{"message":"bad"}}`, want{isReply: true, invalid: true}},
		{"codex error and result", parseCodexReply, `{"id":"3","error":{"code":1},"result":{}}`, want{isReply: true, invalid: true}},
		{"codex numeric id", parseCodexReply, `{"id":3,"result":{}}`, want{isReply: true, invalid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.frame), &m); err != nil {
				t.Fatal(err)
			}
			id, r, isReply, err := tc.parse(m)
			if isReply != tc.want.isReply {
				t.Fatalf("isReply = %v, want %v", isReply, tc.want.isReply)
			}
			if tc.want.invalid {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("a malformed reply was accepted: %q %+v %v", id, r, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a valid frame was refused: %v", err)
			}
			if id != tc.want.id || string(r.body) != tc.want.body || !errors.Is(r.err, tc.want.err) || (tc.want.err == nil && r.err != nil) {
				t.Fatalf("got id %q body %s err %v, want %+v", id, r.body, r.err, tc.want)
			}
		})
	}
}
