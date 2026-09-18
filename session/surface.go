package session

import (
	"encoding/json"
	"sort"
	"strings"
)

// What a restricted session is allowed to have, and how that is judged.
//
// The rules here come from what the installed CLIs actually send, checked
// against them with dummy credentials and a provider that refuses to infer:
//
//   - Claude puts the hosted tools in the request's top-level `tools`, and also
//     makes auxiliary requests with no tools at all — naming the session, for
//     instance. A request with no tools cannot disclose anything, so requiring
//     every request to carry the hosted set would reject a safe session.
//   - Codex puts tool definitions under `input[].type == "additional_tools"`,
//     groups some of them into `namespace` entries, and always defers MCP tools
//     behind a `tool_search` entry. The hosted tools are therefore not in the
//     request at all, and demanding them there would reject every Codex session.
//
// So the judgement is split. Nothing outside the permitted set may appear
// anywhere — that is the part that keeps a disclosure path out. And the hosted
// surface must be positively proven, either by a request that carries it or,
// where the harness defers it, by the tool channel having served it.

// mediatedTools are entries that reach nothing except configured MCP servers.
// They are permitted because a restricted session configures exactly one — the
// caller's — and that server answers every resource method with
// method-not-supported, so there is nothing for them to read.
var mediatedTools = map[string]bool{
	"tool_search":                 true,
	"list_mcp_resources":          true,
	"list_mcp_resource_templates": true,
	"read_mcp_resource":           true,
}

// wireTool is one tool a request carried, kept as identity rather than as a
// bare string. A harness's own tool can be called `inspect` just as easily as a
// caller's, so a name that arrived without this session's server prefix is
// never treated as this session's tool no matter what it spells.
type wireTool struct {
	// name is what the request carried, with this session's prefix removed
	// where there was one.
	name string
	// hosted records that the name arrived under this session's server prefix.
	hosted bool
}

// requestSurface is one outbound request's tool surface.
type requestSurface struct{ tools []wireTool }

// readSurface extracts the tool surface from a provider request, accepting both
// shapes the installed CLIs use. An unreadable body is reported as such; a body
// with no tools is a legitimate auxiliary request, not a failure.
func readSurface(body []byte, server string) (requestSurface, bool) {
	var request struct {
		Tools []json.RawMessage `json:"tools"`
		Input []struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return requestSurface{}, false
	}
	var out requestSurface
	collect := func(raw []json.RawMessage) {
		for _, entry := range raw {
			for _, name := range flattenTool(entry) {
				out.tools = append(out.tools, identify(name, server))
			}
		}
	}
	collect(request.Tools)
	for _, item := range request.Input {
		if item.Type == "additional_tools" {
			collect(item.Tools)
		}
	}
	sort.Slice(out.tools, func(i, j int) bool { return out.tools[i].name < out.tools[j].name })
	return out, true
}

// flattenTool names one tool entry, descending into a namespace group. An entry
// with neither a name nor a type is reported as an explicit unknown rather than
// skipped: an unreadable tool is not an absent one.
func flattenTool(raw json.RawMessage) []string {
	var entry struct {
		Name  string            `json:"name"`
		Type  string            `json:"type"`
		Tools []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return []string{"unreadable_tool_entry"}
	}
	if len(entry.Tools) > 0 {
		var out []string
		for _, nested := range entry.Tools {
			out = append(out, flattenTool(nested)...)
		}
		return out
	}
	if entry.Name != "" {
		return []string{entry.Name}
	}
	if entry.Type != "" {
		return []string{entry.Type}
	}
	return []string{"unreadable_tool_entry"}
}

// identify strips this session's server prefix and records whether there was
// one. A bare name keeps its full text and is not hosted, so a built-in that
// happens to share a caller's tool name is an unauthorized tool, not a match.
func identify(name, server string) wireTool {
	for _, prefix := range []string{"mcp__" + server + "__", server + "__", "mcp__" + server + ".", server + "."} {
		if after, found := strings.CutPrefix(name, prefix); found && after != "" {
			return wireTool{name: after, hosted: true}
		}
	}
	return wireTool{name: name}
}

// judgeSurfaces decides a whole probe. proven says the tool channel served this
// session's tools during the check, which is the only positive evidence
// available when a harness defers them.
func judgeSurfaces(engine string, hosted []string, surfaces []requestSurface, proven bool) *CapabilityError {
	want := map[string]bool{}
	for _, name := range hosted {
		want[name] = true
	}
	extra := map[string]bool{}
	carried := false
	for _, surface := range surfaces {
		seen := map[string]bool{}
		for _, tool := range surface.tools {
			switch {
			case tool.hosted && want[tool.name]:
				seen[tool.name] = true
			case tool.hosted:
				// Arrived under this session's prefix but is not one of its tools.
				extra["mcp__unknown__"+tool.name] = true
			case engine == string(Codex) && mediatedTools[tool.name]:
				// An MCP-mediated helper, permitted because it can address nothing
				// but the one configured server, which serves no resources.
			default:
				extra[tool.name] = true
			}
		}
		if len(want) > 0 && len(seen) == len(want) {
			carried = true
		}
	}
	if len(extra) > 0 {
		names := make([]string, 0, len(extra))
		for name := range extra {
			names = append(names, name)
		}
		sort.Strings(names)
		return &CapabilityError{Engine: engine, Code: CapabilityNativeToolsPresent, Phase: BeforeLaunch, Tools: names}
	}
	if carried || proven {
		return nil
	}
	return &CapabilityError{Engine: engine, Code: CapabilityHostedToolsMissing, Phase: BeforeLaunch, Tools: hosted}
}
