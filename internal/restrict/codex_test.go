package restrict

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const catalog = `{"models":[
 {"slug":"other","supported_reasoning_levels":[{"effort":"low"}]},
 {"slug":"picked","default_reasoning_level":"medium","context_window":400000,
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],
  "shell_type":"local","apply_patch_tool_type":"freeform",
  "experimental_supported_tools":["shell","browser"],"tool_mode":"experimental",
  "node_repl_disabled":false,"base_instructions":"native coding instructions"}]}`

func selected(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var out struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Models) != 1 {
		t.Fatalf("expected exactly the selected model: %s", raw)
	}
	return out.Models[0]
}

func TestCodexCatalogRemovesNativeToolSurfaces(t *testing.T) {
	raw, err := CodexCatalog([]byte(catalog), "picked", "high", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := selected(t, raw)
	for field, want := range map[string]string{
		"shell_type": `"disabled"`, "apply_patch_tool_type": "null",
		"experimental_supported_tools": "[]", "tool_mode": `"standard"`,
		"node_repl_disabled": "true",
	} {
		if string(m[field]) != want {
			t.Errorf("%s = %s, want %s", field, m[field], want)
		}
	}
	// Identity and capability must survive exactly: no substitution, no fallback.
	if string(m["slug"]) != `"picked"` || string(m["context_window"]) != "400000" {
		t.Errorf("model identity changed: %s", raw)
	}
	if string(m["supported_reasoning_levels"]) == "" {
		t.Error("effort levels dropped")
	}
}

// A restricted session keeps the CLI's own coding instructions: removing the
// tools is the restriction, and replacing the prompt is a separate decision.
func TestCodexCatalogKeepsBaseInstructionsUnlessReplaced(t *testing.T) {
	raw, err := CodexCatalog([]byte(catalog), "picked", "low", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(selected(t, raw)["base_instructions"]) != `"native coding instructions"` {
		t.Errorf("base instructions changed: %s", raw)
	}
	replacement := "application reasoning engine"
	raw, err = CodexCatalog([]byte(catalog), "picked", "low", &replacement)
	if err != nil {
		t.Fatal(err)
	}
	if string(selected(t, raw)["base_instructions"]) != `"application reasoning engine"` {
		t.Errorf("replacement not applied: %s", raw)
	}
}

func TestCodexCatalogRejections(t *testing.T) {
	for name, tc := range map[string]struct {
		data, model, effort, code string
	}{
		"unparsable":        {"not json", "picked", "low", InvalidCatalog},
		"absent model":      {catalog, "missing", "low", ModelNotInCatalog},
		"unsupported":       {catalog, "picked", "minimal", UnsupportedEffort},
		"no effort catalog": {`{"models":[{"slug":"picked"}]}`, "picked", "low", MissingEffort},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CodexCatalog([]byte(tc.data), tc.model, tc.effort, nil)
			var reason *Error
			if !errors.As(err, &reason) || reason.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
		})
	}
}

func TestCodexCatalogEffortReadsDefault(t *testing.T) {
	if got := CodexCatalogEffort([]byte(catalog), "picked"); got != "medium" {
		t.Errorf("default effort = %q", got)
	}
	if got := CodexCatalogEffort([]byte(catalog), "absent"); got != "" {
		t.Errorf("unknown model reported effort %q", got)
	}
	if got := CodexCatalogEffort([]byte("not json"), "picked"); got != "" {
		t.Errorf("unparsable catalog reported effort %q", got)
	}
}

// The settings are shared by completion and sessions, so a probe and the launch
// it authorizes must build the identical list.
func TestCodexSettingsAreDeterministicAndCoverEveryFeature(t *testing.T) {
	first, second := CodexSettings(), CodexSettings()
	if len(first) != len(second) {
		t.Fatalf("unstable length %d vs %d", len(first), len(second))
	}
	seen := map[string]bool{}
	for i, setting := range first {
		if setting != second[i] {
			t.Fatalf("unstable order at %d: %q vs %q", i, setting, second[i])
		}
		if seen[setting] {
			t.Fatalf("duplicate setting %q", setting)
		}
		seen[setting] = true
	}
	for _, feature := range CodexFeatures {
		if !seen["features."+feature+"=false"] {
			t.Errorf("feature %q is not disabled", feature)
		}
	}
	for _, required := range []string{`approval_policy="never"`, `web_search="disabled"`, `project_doc_max_bytes=0`, `agents.enabled=false`} {
		if !seen[required] {
			t.Errorf("missing %q", required)
		}
	}
	if strings.Contains(strings.Join(first, " "), "danger") {
		t.Error("restricted settings must not name a bypass mode")
	}
}
