package completion

import (
	"encoding/json"
	"strings"
	"testing"
)

// Measured against the installed CLI using an isolated dummy home and a local
// synthetic provider: only StructuredOutput was advertised, an invented
// read_file call got this exact no-such-tool receipt, then output succeeded.
// Nothing in these tests invokes a CLI, account or external service.
func TestClaudeUnavailableToolRecovery(t *testing.T) {
	init := `{"type":"system","subtype":"init","tools":["StructuredOutput"]}`
	call := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"unavailable-1","name":"read_file","input":{"path":"secret"}}]}}`
	denied := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unavailable-1","is_error":true,"content":"<tool_use_error>Error: No such tool available: read_file</tool_use_error>"}]}}`
	structured := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"structured-1","name":"StructuredOutput","input":{}}]}}`
	terminal := `{"type":"result","subtype":"success","structured_output":{"content":"Recovered","tool_calls":[]},"usage":{"input_tokens":10,"output_tokens":5}}`
	for _, tc := range []struct {
		name   string
		events []string
		code   string
	}{
		{"explicitly unavailable", []string{init, call, denied, structured, terminal}, ""},
		{"native tool advertised", []string{strings.Replace(init, `"StructuredOutput"`, `"StructuredOutput","Read"`, 1), terminal}, "unexpected_native_tool_catalog"},
		{"unacknowledged", []string{init, call, terminal}, "unexpected_native_tool_call"},
		{"generic error could have effects", []string{init, call, strings.Replace(denied, "<tool_use_error>Error: No such tool available: read_file</tool_use_error>", "permission denied", 1), terminal}, "unexpected_native_tool_call"},
		{"successful tool", []string{init, call, strings.Replace(denied, `"is_error":true`, `"is_error":false`, 1), terminal}, "unexpected_native_tool_call"},
		{"wrong receipt ID", []string{init, call, strings.Replace(denied, "unavailable-1", "another", 1), terminal}, "unexpected_native_tool_call"},
		{"wrong receipt name", []string{init, call, strings.Replace(denied, "read_file", "write_file", 1), terminal}, "unexpected_native_tool_call"},
		{"no catalog", []string{call, denied, terminal}, "unexpected_native_tool_call"},
		{"late catalog", []string{call, init, denied, terminal}, "unexpected_native_tool_call"},
		{"duplicate catalog", []string{init, init, terminal}, "unexpected_native_tool_catalog"},
		{"missing call ID", []string{init, strings.Replace(call, `"id":"unavailable-1",`, "", 1), denied, terminal}, "unexpected_native_tool_call"},
		{"reused ID", []string{init, call, denied, call, denied, terminal}, "unexpected_native_tool_call"},
		{"call after terminal", []string{init, terminal, call}, "unexpected_native_tool_call"},
		{"contradictory receipt", []string{init, call, denied, strings.Replace(denied, `"is_error":true`, `"is_error":false`, 1), terminal}, "unexpected_native_tool_call"},
		{"duplicate receipt", []string{init, call, denied, denied, terminal}, "unexpected_native_tool_call"},
		{"orphan receipt", []string{init, denied, terminal}, "unexpected_native_tool_call"},
		{"not recovered", []string{init, call, denied}, "missing_terminal_result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, usage, err := parseClaude([]byte(strings.Join(tc.events, "\n")), nil)
			if tc.code == "" {
				if err != nil || msg.Content != "Recovered" {
					t.Fatalf("message=%+v err=%v", msg, err)
				}
			} else {
				if err == nil {
					t.Fatal("accepted unverified response")
				}
				f, ok := err.(*RequestError)
				if !ok || f.Code != tc.code {
					t.Fatalf("want %s, got %v", tc.code, err)
				}
				if msg.Content != "" || len(msg.ToolCalls) != 0 {
					t.Fatal("failed validation returned actions")
				}
				encoded, _ := json.Marshal(err)
				if strings.Contains(string(encoded)+err.Error(), "secret") {
					t.Fatal("diagnostic leaks response")
				}
			}
			wantKnown := tc.name != "not recovered"
			if usage.Known != wantKnown || (wantKnown && usage.TotalTokens != 15) {
				t.Fatalf("terminal accounting lost: %+v", usage)
			}
		})
	}
}

func TestClaudeInvalidEnvelopePreservesUsage(t *testing.T) {
	_, usage, err := parseClaude([]byte(`{"type":"result","subtype":"success","structured_output":{},"usage":{"input_tokens":10,"output_tokens":5}}`), nil)
	if err == nil || !usage.Known || usage.TotalTokens != 15 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}
