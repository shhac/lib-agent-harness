package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tool with no required arguments, as a Go caller naturally builds it, used
// to reach the harness as "required": null. Claude Code 2.1.283 rejects the
// whole tools/list for that, so a restricted session started with none of its
// hosted tools. Every hosted tool now reaches the harness as a valid schema.
func TestHostedSchemasNeverCarryNull(t *testing.T) {
	var required []string
	tools := freezeTools([]ToolDefinition{
		{Name: "list_connections", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "required": required, "additionalProperties": false}},
		{Name: "read_task", Schema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "enum": []string(nil), "description": nil}}, "required": []string{"id"}}},
	})
	h := &toolHost{cfg: ToolHost{Tools: tools}}
	wire, err := json.Marshal(h.list())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "null") {
		t.Fatalf("a schema keyword reached the harness as null: %s", wire)
	}
	var listed struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(wire, &listed); err != nil {
		t.Fatal(err)
	}
	if got, ok := listed.Tools[0].InputSchema["required"].([]any); !ok || len(got) != 0 {
		t.Fatalf("an empty required list should be an empty array: %s", wire)
	}
	if got := listed.Tools[1].InputSchema["required"].([]any); len(got) != 1 || got[0] != "id" {
		t.Fatalf("required arguments were lost: %s", wire)
	}
}
