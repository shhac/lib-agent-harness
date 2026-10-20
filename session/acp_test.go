package session

import (
	"encoding/json"
	"testing"
)

// permissionRequest is a session/request_permission request from the agent,
// carrying params.
func permissionRequest(params string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"jsonrpc": json.RawMessage(`"2.0"`), "id": json.RawMessage(`7`), "method": json.RawMessage(`"session/request_permission"`), "params": json.RawMessage(params)}
}

// answerOutcome is what a dialect answered a permission request with.
func answerOutcome(t *testing.T, d dialect, params string) map[string]any {
	t.Helper()
	reply, ok := d.answer(permissionRequest(params))
	if !ok {
		t.Fatal("a permission request was not recognised")
	}
	result, _ := reply["result"].(map[string]any)
	outcome, _ := result["outcome"].(map[string]any)
	if outcome == nil {
		t.Fatalf("no outcome: %v", reply)
	}
	return outcome
}

// Command Code and fx answer permission requests the same strict way, under
// either policy, through their own dialect.
func TestStrictPermissionNeverGrantsMoreThanOnce(t *testing.T) {
	dialects := map[string]map[bool]dialect{
		"command code": {false: commandCodeDialect(Policy{CommandCodePermission: CommandCodeDenyWhenAsked}), true: commandCodeDialect(Policy{CommandCodePermission: CommandCodeAllowWhenAsked})},
		"fx":           {false: fxDialect(Policy{FxPermission: FxDenyWhenAsked}), true: fxDialect(Policy{FxPermission: FxAllowWhenAsked})},
	}
	once := `{"options":[{"optionId":"allow_once","kind":"allow_once"},{"optionId":"allow_always","kind":"allow_always"},{"optionId":"reject_once","kind":"reject_once"}]}`
	for name, byPolicy := range dialects {
		for allow, d := range byPolicy {
			want := "reject_once"
			if allow {
				want = "allow_once"
			}
			if got := answerOutcome(t, d, once); got["outcome"] != "selected" || got["optionId"] != want {
				t.Errorf("%s allow=%v: %v", name, allow, got)
			}
			for _, cancelled := range []string{
				`{"options":[{"optionId":"allow_always","kind":"allow_always"},{"optionId":"reject_always","kind":"reject_always"}]}`,
				`{"options":[{"optionId":"allow_once","kind":"allow_once"}]}`,
				`{"options":[{"optionId":"reject_once","kind":"reject_once"}]}`,
				`{"options":[{"optionId":"a","kind":"allow_once"},{"optionId":"b","kind":"allow_once"},{"optionId":"r","kind":"reject_once"}]}`,
				`{"options":[{"optionId":"same","kind":"allow_once"},{"optionId":"same","kind":"reject_once"}]}`,
				`{"options":[{"optionId":"","kind":"allow_once"},{"optionId":"r","kind":"reject_once"}]}`,
				`not json`,
			} {
				if got := answerOutcome(t, d, cancelled); got["outcome"] != "cancelled" {
					t.Errorf("%s allow=%v answered %s with %v", name, allow, cancelled, got)
				}
			}
			modeSwitch := `{"toolCall":{"kind":"switch_mode"},"options":[{"optionId":"allow_once","kind":"allow_once"},{"optionId":"reject_once","kind":"reject_once"}]}`
			if got := answerOutcome(t, d, modeSwitch); got["optionId"] != "reject_once" {
				t.Errorf("%s allow=%v let a mode switch through: %v", name, allow, got)
			}
			reply, ok := d.answer(map[string]json.RawMessage{"id": json.RawMessage(`3`), "method": json.RawMessage(`"terminal/create"`)})
			if !ok || reply["error"] == nil || reply["result"] != nil || reply["jsonrpc"] != "2.0" {
				t.Errorf("%s allow=%v did not refuse an unhandled request: %v", name, allow, reply)
			}
		}
	}
}

// Grok keeps its own once-only selection (design-docs/2026-10-04-engine-
// adapters.md, invariant 3): it takes the first option of the kind its policy
// wants, and needs no reject option to allow. This pins that difference so a
// later change to it is deliberate.
func TestGrokPermissionSelectionIsItsOwn(t *testing.T) {
	allow := grokDialect(Policy{GrokPermission: GrokAllowWhenAsked})
	if got := answerOutcome(t, allow, `{"options":[{"optionId":"allow_once","kind":"allow_once"}]}`); got["optionId"] != "allow_once" {
		t.Errorf("an allow-only request: %v", got)
	}
	if got := answerOutcome(t, allow, `{"options":[{"optionId":"a","kind":"allow_once"},{"optionId":"b","kind":"allow_once"}]}`); got["optionId"] != "a" {
		t.Errorf("a repeated kind: %v", got)
	}
	deny := grokDialect(Policy{GrokPermission: GrokDenyWhenAsked})
	if got := answerOutcome(t, deny, `{"options":[{"optionId":"allow_once","kind":"allow_once"}]}`); got["outcome"] != "cancelled" {
		t.Errorf("deny chose an allow: %v", got)
	}
}

// A tool is named by an identifier its engine states, never by text that
// could carry a path or a command.
func TestACPToolNamesAreIdentifiers(t *testing.T) {
	for name, c := range map[string]struct {
		read   func(map[string]json.RawMessage) string
		update string
		want   string
	}{
		"fx name":              {fxToolName, `{"name":"write_file"}`, "write_file"},
		"fx command as a name": {fxToolName, `{"name":"rm -rf /tmp/x"}`, ""},
		"command code title":   {commandCodeToolName, `{"title":"Read /etc/passwd","kind":"read"}`, "read"},
		"grok title":           {grokToolName, `{"title":"cat /etc/passwd"}`, "tool"},
	} {
		var update map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c.update), &update); err != nil {
			t.Fatal(err)
		}
		if got := c.read(update); got != c.want {
			t.Errorf("%s: %q", name, got)
		}
	}
}
