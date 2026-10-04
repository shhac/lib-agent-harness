package sandbox

// Definition describes one hosted file tool without authorizing its execution.
type Definition struct {
	Name, Description string
	Schema            map[string]any
}

// Definitions returns only offered tools, including mutation when write is true.
func Definitions(write bool) []Definition {
	defs := []Definition{
		{
			Name:        workbenchReadFile,
			Description: "Read a UTF-8 text file in the workspace. Returns at most 2000 lines and 64 KiB per call, and says where to continue.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "description": "Relative, slash-separated path inside the workspace."},
					"offset": map[string]any{"type": "integer", "description": "First line to return, from 1. Default 1."},
					"limit":  map[string]any{"type": "integer", "description": "Most lines to return, up to 2000. Default 2000."},
				},
				"required":             []any{"path"},
				"additionalProperties": false,
			},
		},
		{
			Name:        workbenchListFiles,
			Description: "List a workspace directory, sorted, at most 2000 entries. Directories end in /; links, FIFOs and other files are marked. .git is skipped.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string", "description": "Relative, slash-separated directory inside the workspace. Default: the workspace itself."},
					"depth": map[string]any{"type": "integer", "description": "Levels to descend, 1 to 8. Default 2."},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        workbenchSearchFiles,
			Description: "Search workspace UTF-8 files using RE2 or literal text. At most 200 matches, lines cut to 400 bytes; files over 1 MiB, binary files, links and .git are skipped.",
			Schema: map[string]any{"type": "object", "properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "RE2 pattern, at most 4096 bytes."},
				"literal": map[string]any{"type": "boolean"},
				"path":    map[string]any{"type": "string", "description": "Relative file or directory. Default: workspace."},
				"glob":    map[string]any{"type": "string", "description": "Slash-relative glob, or basename glob without a slash."},
			}, "required": []any{"pattern"}, "additionalProperties": false},
		},
	}
	if write {
		defs = append(defs, workbenchWriteDefinitions()...)
	}
	offered := defs[:0]
	for _, d := range defs {
		if ToolAvailability(d.Name) == "" {
			offered = append(offered, d)
		}
	}
	return offered
}
