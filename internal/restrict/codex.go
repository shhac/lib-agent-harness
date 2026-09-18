// Package restrict holds the provider mechanics that remove a native CLI's own
// tool surface. Both the constrained completion transport and restricted native
// sessions depend on it, so the two cannot drift apart: a feature switch added
// for one is added for both.
//
// Restricting writes is not what this package does. A tool that can read is a
// disclosure path regardless of whether it can write, so the restriction is the
// removal of the tools themselves. Nothing here is evidence on its own: callers
// verify the result against the installed CLI before trusting it.
package restrict

import (
	"encoding/json"
	"strconv"
)

// Error carries a fixed reason code. Provider text never enters it.
type Error struct{ Code string }

func (e *Error) Error() string { return "restricted launch: " + e.Code }

// Reason codes returned by CodexCatalog.
const (
	InvalidCatalog     = "invalid_model_catalog"
	MissingEffort      = "missing_effort_catalog"
	UnsupportedEffort  = "unsupported_effort"
	ModelNotInCatalog  = "model_not_in_catalog"
	InvalidInstruction = "invalid_base_instructions"
)

// CodexFeatures names every native surface disabled by configuration. Adding a
// value here restricts completion and sessions together.
var CodexFeatures = []string{"shell_tool", "unified_exec", "apps", "plugins", "hooks", "multi_agent", "multi_agent_v2", "browser_use", "browser_use_external", "computer_use", "image_generation", "code_mode", "code_mode_host", "goals", "sleep_tool", "view_image", "workspace_dependencies", "memories", "skill_search", "skill_mcp_dependency_install", "shell_snapshot", "unbounded_connection_retries", "remote_plugin", "tool_suggest"}

// CodexSettings returns the `-c` overrides that disable native execution,
// inherited instructions and unrelated surfaces. They are ordered
// deterministically so a probe and the launch it authorizes are comparable.
func CodexSettings() []string {
	settings := []string{
		`approval_policy="never"`, `web_search="disabled"`, `project_doc_max_bytes=0`,
		`tools.update_plan.enabled=false`, `tools.experimental_request_user_input.enabled=false`,
		`features.skip_host_skill_discovery=true`, `agents.enabled=false`,
		`include_environment_context=false`, `include_apps_instructions=false`,
		`include_collaboration_mode_instructions=false`, `include_permissions_instructions=false`,
		`check_for_update_on_startup=false`, `analytics.enabled=false`,
	}
	for _, feature := range CodexFeatures {
		settings = append(settings, "features."+feature+"=false")
	}
	return settings
}

// CodexCatalog narrows an installed model catalog to the selected model with
// its native execution surfaces removed. The model's identity, capabilities and
// effort levels are preserved exactly: no substitution and no fallback.
//
// baseInstructions replaces the model's default prompt when non-nil. A
// restricted session leaves it nil, because removing the tools is the
// restriction and the native coding instructions remain the right starting
// point for a session whose tools arrive from its caller instead.
func CodexCatalog(data []byte, model, effort string, baseInstructions *string) ([]byte, error) {
	var catalog struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(data, &catalog) != nil {
		return nil, &Error{InvalidCatalog}
	}
	for _, m := range catalog.Models {
		var slug string
		_ = json.Unmarshal(m["slug"], &slug)
		if slug != model {
			continue
		}
		var levels []struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(m["supported_reasoning_levels"], &levels) != nil {
			return nil, &Error{MissingEffort}
		}
		supported := false
		for _, level := range levels {
			if level.Effort == effort {
				supported = true
			}
		}
		if !supported {
			return nil, &Error{UnsupportedEffort}
		}
		m["shell_type"] = json.RawMessage(`"disabled"`)
		m["apply_patch_tool_type"] = json.RawMessage(`null`)
		m["experimental_supported_tools"] = json.RawMessage(`[]`)
		m["tool_mode"] = json.RawMessage(`"standard"`)
		m["node_repl_disabled"] = json.RawMessage(`true`)
		if baseInstructions != nil {
			m["base_instructions"] = json.RawMessage(strconv.Quote(*baseInstructions))
		}
		return json.Marshal(map[string]any{"models": []map[string]json.RawMessage{m}})
	}
	return nil, &Error{ModelNotInCatalog}
}

// CodexCatalogEffort reports the catalog's default reasoning level for a model,
// or an empty string when the catalog does not describe one.
func CodexCatalogEffort(data []byte, model string) string {
	var catalog struct {
		Models []struct {
			Slug    string `json:"slug"`
			Default string `json:"default_reasoning_level"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &catalog) != nil {
		return ""
	}
	for _, m := range catalog.Models {
		if m.Slug == model {
			return m.Default
		}
	}
	return ""
}
