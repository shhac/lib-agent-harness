package session

import (
	"encoding/json"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Every CLI engine has a complete dialect, and nothing else gets one.
func TestEveryCLIEngineHasADialect(t *testing.T) {
	for _, e := range harness.Engines() {
		d, ok := dialectOf(Options{Provider: harness.Provider{Engine: e}})
		if e.Transport() != harness.CLITransport {
			if ok {
				t.Errorf("%s: an API engine has a process dialect", e)
			}
			continue
		}
		if !ok || d.envelope == nil || d.parseReply == nil || d.answer == nil {
			t.Errorf("%s: incomplete dialect", e)
		}
	}
}

func dialectFor(t *testing.T, e harness.Engine, p Policy) dialect {
	t.Helper()
	d, ok := dialectOf(Options{Provider: harness.Provider{Engine: e}, Policy: p})
	if !ok {
		t.Fatalf("%s has no dialect", e)
	}
	return d
}

func frame(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func encoded(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The bytes each engine is sent for a request are pinned exactly.
func TestDialectEnvelopes(t *testing.T) {
	for e, want := range map[harness.Engine]string{
		harness.Codex:       `{"id":"7","method":"thread/start","params":{"cwd":"/w"}}`,
		harness.Claude:      `{"request":{"cwd":"/w","subtype":"thread/start"},"request_id":"7","type":"control_request"}`,
		harness.Grok:        `{"id":"7","jsonrpc":"2.0","method":"thread/start","params":{"cwd":"/w"}}`,
		harness.CommandCode: `{"id":"7","jsonrpc":"2.0","method":"thread/start","params":{"cwd":"/w"}}`,
	} {
		params := map[string]any{"cwd": "/w"}
		if got := encoded(t, dialectFor(t, e, Policy{}).envelope("7", "thread/start", params)); got != want {
			t.Errorf("%s: %s", e, got)
		}
		if len(params) != 1 {
			t.Errorf("%s: the caller's params were changed", e)
		}
	}
}

// Every server request is answered, and every unnamed operation is refused,
// with exactly these bytes; frames that are not requests are left alone.
func TestDialectAnswers(t *testing.T) {
	refused := `"error":{"code":-32601,"message":"Client does not authorize this operation"}`
	for _, tc := range []struct {
		name   string
		engine harness.Engine
		policy Policy
		frame  string
		want   string
	}{
		{"codex unknown", harness.Codex, Policy{}, `{"id":5,"method":"fs/write"}`, `{` + refused + `,"id":5}`},
		{"codex command approval", harness.Codex, Policy{}, `{"id":5,"method":"item/commandExecution/requestApproval"}`, `{"id":5,"result":{"decision":"decline"}}`},
		{"codex file approval", harness.Codex, Policy{}, `{"id":"a","method":"item/fileChange/requestApproval"}`, `{"id":"a","result":{"decision":"decline"}}`},
		{"codex permissions", harness.Codex, Policy{}, `{"id":5,"method":"item/permissions/requestApproval"}`, `{"id":5,"result":{"permissions":{},"scope":"turn"}}`},
		{"codex elicitation", harness.Codex, Policy{}, `{"id":5,"method":"mcpServer/elicitation/request"}`, `{"id":5,"result":{"action":"decline","content":null}}`},
		{"codex notification", harness.Codex, Policy{}, `{"method":"turn/started"}`, ``},
		{"codex reply", harness.Codex, Policy{}, `{"id":5,"result":{}}`, ``},
		{"claude control request", harness.Claude, Policy{}, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`, `{"response":{"error":"Client does not authorize this operation","request_id":"r1","subtype":"error"},"type":"control_response"}`},
		{"claude other frame", harness.Claude, Policy{}, `{"type":"assistant","id":1,"method":"x"}`, ``},
		{"grok unknown", harness.Grok, Policy{}, `{"jsonrpc":"2.0","id":3,"method":"fs/write_text_file"}`, `{` + refused + `,"id":3,"jsonrpc":"2.0"}`},
		{"grok notification", harness.Grok, Policy{}, `{"jsonrpc":"2.0","method":"session/update"}`, ``},
		{"command code unknown", harness.CommandCode, Policy{}, `{"jsonrpc":"2.0","id":3,"method":"terminal/create"}`, `{` + refused + `,"id":3,"jsonrpc":"2.0"}`},
		{"command code notification", harness.CommandCode, Policy{}, `{"jsonrpc":"2.0","method":"session/update"}`, ``},
	} {
		reply, isRequest := dialectFor(t, tc.engine, tc.policy).answer(frame(t, tc.frame))
		if tc.want == "" {
			if isRequest {
				t.Errorf("%s: answered %v", tc.name, reply)
			}
			continue
		}
		if !isRequest {
			t.Errorf("%s: not answered", tc.name)
			continue
		}
		if got := encoded(t, reply); got != tc.want {
			t.Errorf("%s: %s", tc.name, got)
		}
	}
}

// Two sessions of one engine with opposite permission policies answer the same
// request by their own policy: the policy belongs to the wire, not the engine.
func TestDialectPermissionPolicyIsPerWire(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"toolCall":{"kind":"edit"},"options":[{"optionId":"allow","kind":"allow_once"},{"optionId":"deny","kind":"reject_once"}]}}`
	for _, tc := range []struct {
		engine      harness.Engine
		allow, deny Policy
	}{
		{harness.Grok, Policy{GrokPermission: GrokAllowWhenAsked}, Policy{GrokPermission: GrokDenyWhenAsked}},
		{harness.CommandCode, Policy{CommandCodePermission: CommandCodeAllowWhenAsked}, Policy{CommandCodePermission: CommandCodeDenyWhenAsked}},
	} {
		allowing, denying := dialectFor(t, tc.engine, tc.allow), dialectFor(t, tc.engine, tc.deny)
		for i := 0; i < 2; i++ {
			allowed, _ := allowing.answer(frame(t, request))
			denied, _ := denying.answer(frame(t, request))
			if got := encoded(t, allowed); got != `{"id":9,"jsonrpc":"2.0","result":{"outcome":{"optionId":"allow","outcome":"selected"}}}` {
				t.Errorf("%s allowing: %s", tc.engine, got)
			}
			if got := encoded(t, denied); got != `{"id":9,"jsonrpc":"2.0","result":{"outcome":{"optionId":"deny","outcome":"selected"}}}` {
				t.Errorf("%s denying: %s", tc.engine, got)
			}
		}
	}
}
