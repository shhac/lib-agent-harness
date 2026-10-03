package session

// The API workbench: the library's own file tools over an OpenAI-compatible
// session's WorkDir (design-docs/2026-09-29-api-workbench.md). What the model
// is offered and what the host admits both come from workbenchDefinitions, so
// the two lists cannot drift apart. The tools themselves are in
// workbench_files.go.
//
// Read tools are always present. Writes are opt-in and atomic; shell commands
// require a pre-launch proof of the pinned operating-system sandbox.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
)

// Workbench gives an OpenAI-compatible session the library's own tools over
// Options.WorkDir. The zero value asks for read_file, list_files and
// search_files, which run in the library's process, confined to WorkDir.
//
// Reads require singly linked regular files on the workspace mount.
type Workbench struct {
	standaloneCommands bool     // set only by OpenCommandSandbox
	commandStateDir    string   // standalone recovery state; sessions retain their paths
	system             []string // the pinned set selected and proved for this launch
	commandBinary      string   // selected by the pre-launch command proof
	commandIdentity    string   // evidence must still name the binary being launched
	// Write adds atomic write_file and edit_file tools.
	Write bool
	// NewFileMode defaults to 0600. Windows uses the parent directory ACL.
	NewFileMode fs.FileMode
	Commands    *Commands
}

// Commands configures sandboxed shell execution.
type Commands struct {
	Loopback bool
	Read     []string
	Env      []string
	Timeout  time.Duration
}

// The workbench's tool names. All six are reserved whenever a workbench is
// set, including those its options do not switch on, so no later tool can
// collide with a caller's.
const (
	workbenchReadFile    = "read_file"
	workbenchListFiles   = "list_files"
	workbenchSearchFiles = "search_files"
	workbenchWriteFile   = "write_file"
	workbenchEditFile    = "edit_file"
	workbenchRunCommand  = "run_command"
)

var workbenchReserved = []string{workbenchReadFile, workbenchListFiles, workbenchSearchFiles, workbenchWriteFile, workbenchEditFile, workbenchRunCommand}

// RefusedWorkbenchToolReserved: a caller tool bears one of the workbench's
// reserved names. The host keys tools by name, so it would otherwise be
// silently replaced by the library's.
const RefusedWorkbenchToolReserved = "workbench_tool_name_reserved"

func isWorkbenchTool(name string) bool {
	for _, reserved := range workbenchReserved {
		if name == reserved {
			return true
		}
	}
	return false
}

// refuseCLIWorkbench refuses a workbench for an engine that brings its own
// tools.
func refuseCLIWorkbench(o Options) error {
	if o.Workbench == nil {
		return nil
	}
	return refuse(o, "workbench", RefusedNotOffered, "the workbench is for OpenAI-compatible sessions; a CLI engine's own tools and Sandbox cover this")
}

// normalizeWorkbench checks a workbench's WorkDir, its separation from
// RuntimeHome and the reserved tool names. It runs after RuntimeHome and
// the caller's tools are normalized.
func normalizeWorkbench(o Options) (Options, error) {
	if o.Workbench == nil {
		return o, nil
	}
	if o.Workbench.Commands != nil && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "sandbox", Code: RefusedNotOffered, Capability: harness.Support(o.Provider.Engine, harness.Session, harness.Sandbox)}
	}
	if o.Workbench.NewFileMode & ^fs.FileMode(0666) != 0 {
		return o, refuse(o, "workbench", RefusedLimit, "NewFileMode must contain only permission bits within 0666")
	}
	if o.WorkDir == "" {
		return o, refuse(o, "work_dir", RefusedWorkDir, "a workbench requires WorkDir, the workspace its tools are confined to")
	}
	if !filepath.IsAbs(o.WorkDir) {
		return o, refuse(o, "work_dir", RefusedWorkDir, "WorkDir must be an absolute path")
	}
	work, err := filepath.EvalSymlinks(o.WorkDir)
	if err != nil {
		return o, refuse(o, "work_dir", RefusedWorkDir, "WorkDir must be an existing directory")
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		return o, refuse(o, "work_dir", RefusedWorkDir, "WorkDir must be an existing directory")
	}
	// normalizeRuntimeHome only made it absolute; resolve it here rather than
	// trust that.
	home, err := filepath.EvalSymlinks(o.RuntimeHome)
	if err != nil {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "the runtime home could not be resolved")
	}
	if nested(work, home) || nested(home, work) {
		return o, refuse(o, "work_dir", RefusedWorkDir, "the workspace and the runtime home must not contain one another")
	}
	for _, tool := range o.Restriction.Tools.Tools {
		if isWorkbenchTool(tool.Name) {
			return o, refuse(o, "tools", RefusedWorkbenchToolReserved, "read_file, list_files, search_files, write_file, edit_file and run_command are reserved for the library's workbench tools")
		}
	}
	if limit := o.Restriction.Tools.MaxResultBytes; limit > 0 && limit < minWorkbenchResult {
		return o, refuse(o, "tools", RefusedLimit, "a workbench needs ToolHost.MaxResultBytes of at least 4096, so its results can say where they were cut")
	}
	frozen := *o.Workbench
	if frozen.NewFileMode == 0 {
		frozen.NewFileMode = 0600
	}
	o.Workbench = &frozen
	o.WorkDir = work
	return normalizeWorkbenchCommands(o)
}

// nested reports whether inner is outer or lies inside it. Both are absolute
// and resolved. Paths are compared element by element, without case where the
// file system usually has none; then each of inner's ancestors, inner
// included, is compared with outer by identity, which catches a spelling the
// comparison missed. Failing to read outer counts as nested.
func nested(outer, inner string) bool {
	if lexicallyWithin(outer, inner) {
		return true
	}
	target, err := os.Stat(outer)
	if err != nil {
		return true
	}
	for dir := inner; ; {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, target) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func lexicallyWithin(outer, inner string) bool {
	split := func(p string) []string {
		return strings.FieldsFunc(filepath.Clean(p), func(r rune) bool { return r == filepath.Separator })
	}
	o, i := split(outer), split(inner)
	if filepath.VolumeName(outer) != "" || filepath.VolumeName(inner) != "" {
		if !strings.EqualFold(filepath.VolumeName(outer), filepath.VolumeName(inner)) {
			return false
		}
	}
	if len(o) > len(i) {
		return false
	}
	fold := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	for n := range o {
		if o[n] != i[n] && !(fold && strings.EqualFold(o[n], i[n])) {
			return false
		}
	}
	return true
}

// workbenchDigest is what a resume must match: the workspace and the
// workbench's powers. Env and Timeout change no power, so they stay out. The
// effective new-file mode joins it with Write, which stage 1 does not offer.
func workbenchDigest(o Options) any {
	if o.Workbench == nil {
		return nil
	}
	base := struct {
		WorkDir  string
		Write    bool
		Commands bool
		Loopback bool
		Read     []string
	}{WorkDir: o.WorkDir, Write: o.Workbench.Write}
	if c := o.Workbench.Commands; c != nil {
		base.Commands = true
		base.Loopback = c.Loopback
		base.Read = c.Read
	}
	if !o.Workbench.Write {
		return base
	}
	mode := o.Workbench.NewFileMode
	if mode == 0 {
		mode = 0600
	}
	return struct {
		Base        any
		NewFileMode uint32
	}{base, uint32(mode.Perm())}
}

// workbenchDefinitions are the workbench tools the options switch on. Both
// the request's tools and the host's admitted tools come from here.
func workbenchDefinitions(o Options) []ToolDefinition {
	if o.Workbench == nil {
		return nil
	}
	defs := []ToolDefinition{
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
	if o.Workbench.Write {
		defs = append(defs, workbenchWriteDefinitions()...)
	}
	if o.Workbench.Commands != nil {
		defs = append(defs, workbenchCommandDefinition())
	}
	return defs
}

// completionTools converts definitions to the request's function tools.
func completionTools(defs []ToolDefinition) []completion.Tool {
	tools := make([]completion.Tool, 0, len(defs))
	for _, t := range defs {
		tools = append(tools, completion.Tool{Type: "function", Function: completion.Function{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
	}
	return tools
}

// workbenchHandler answers the workbench's calls, and passes every other call
// on. Only the tools workbenchDefinitions returned are admitted, so no other
// reserved name reaches it.
type workbenchHandler struct {
	ws   *workspace
	next ToolHandler
}

func (h workbenchHandler) CallTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	switch call.Name {
	case workbenchWriteFile, workbenchEditFile:
		r, err := h.ws.dispatchWrite(ctx, func() (ToolResult, error) { return h.ws.writeFile(ctx, call.Name, call.Arguments) })
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return workbenchError(call.Name, wbWriteFailed, ""), nil
		}
		return r, err
	case workbenchRunCommand:
		return h.ws.runCommand(ctx, call.Arguments)
	case workbenchReadFile:
		return h.ws.dispatch(ctx, func() (ToolResult, error) { return h.ws.readFile(ctx, call.Arguments) })
	case workbenchSearchFiles:
		return h.ws.dispatch(ctx, func() (ToolResult, error) { return h.ws.searchFiles(ctx, call.Arguments) })
	case workbenchListFiles:
		return h.ws.dispatch(ctx, func() (ToolResult, error) { return h.ws.listFiles(ctx, call.Arguments) })
	}
	return h.next.CallTool(ctx, call)
}
