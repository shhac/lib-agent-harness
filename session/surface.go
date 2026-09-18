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

// requestSurface is one outbound request's tool surface.
type requestSurface struct {
	// names are the tool identifiers the request carries, namespaces flattened.
	names []string
	// hosted is how many of them are this session's own tools.
	hosted int
}

// readSurface extracts the tool surface from a provider request, accepting both
// shapes the installed CLIs use. An unrecognized shape yields no names, which
// the caller treats as an auxiliary request rather than as proof of anything.
func readSurface(body []byte, server string, hosted map[string]bool) (requestSurface, bool) {
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
				normalized := normalizeWireTool(name, server)
				out.names = append(out.names, normalized)
				if hosted[normalized] {
					out.hosted++
				}
			}
		}
	}
	collect(request.Tools)
	for _, item := range request.Input {
		if item.Type == "additional_tools" {
			collect(item.Tools)
		}
	}
	sort.Strings(out.names)
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

// normalizeWireTool strips the server prefix a harness applies to a hosted tool.
// A bare name is returned unchanged: treating an unprefixed `inspect` as this
// session's hosted `inspect` would let a built-in tool of the same name pass as
// one of ours, which is exactly the confusion worth avoiding.
func normalizeWireTool(name, server string) string {
	for _, prefix := range []string{"mcp__" + server + "__", server + "__", "mcp__" + server + ".", server + "."} {
		if after, found := strings.CutPrefix(name, prefix); found && after != "" {
			return after
		}
	}
	return name
}

// judgeSurfaces decides a whole probe. proven says the tool channel served this
// session's tools during the check, which is the only positive evidence
// available when a harness defers them.
func judgeSurfaces(engine, server string, hosted []string, surfaces []requestSurface, proven bool) *CapabilityError {
	want := map[string]bool{}
	for _, name := range hosted {
		want[name] = true
	}
	permitted := map[string]bool{}
	for name := range want {
		permitted[name] = true
	}
	if engine == string(Codex) {
		for name := range mediatedTools {
			permitted[name] = true
		}
	}
	extra := map[string]bool{}
	carried := false
	for _, surface := range surfaces {
		seen := map[string]bool{}
		for _, name := range surface.names {
			if !permitted[name] {
				extra[name] = true
			}
			if want[name] {
				seen[name] = true
			}
		}
		if len(seen) == len(want) && len(want) > 0 {
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
