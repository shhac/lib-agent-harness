package session

// The API workbench: the library's own file tools over an OpenAI-compatible
// session's WorkDir (design-docs/2026-09-29-api-workbench.md). What the model
// is offered and what the host admits both come from workbenchDefinitions, so
// the two lists cannot drift apart. The tools themselves are in
// sandbox.Workspace.
//
// Only verified file tools are present. Writes are opt-in and atomic; shell commands
// require a pre-launch proof of the pinned operating-system sandbox.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/sandbox"
)

// Workbench gives an OpenAI-compatible session the library's own tools over
// Options.WorkDir. The zero value offers list_files on Linux and macOS;
// read_file and search_files are disabled pending verified workspace admission.
// Capabilities.WorkbenchTools reports the effective configured surface.
type Workbench struct {
	proof sandbox.Proof
	// Write adds atomic write_file. On Windows it also adds edit_file;
	// edit_file is disabled on Linux and macOS pending verified admission.
	Write bool
	// NewFileMode defaults to 0600. Windows uses the parent directory ACL.
	NewFileMode fs.FileMode
	Commands    *Commands
}

// Commands configures sandboxed shell execution.
type Commands struct {
	// Loopback on macOS permits binds and inbound on every local interface.
	Loopback bool
	// LoopbackLocalOnly requires Loopback; unsupported for workbench sessions.
	LoopbackLocalOnly bool
	// LoopbackPorts requests 1–32 selected ports (1–65535); nil leaves Loopback unchanged.
	// Selected-port confinement is refused before discovery or launch on every platform.
	LoopbackPorts []int
	// LoopbackControl is an off-machine IP literal for a selected-port proof.
	// It requires LoopbackPorts; no proof is currently offered.
	LoopbackControl string

	Read    []string
	Env     []string
	Timeout time.Duration
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
	if err := refuseSelectedPorts(o); err != nil {
		return o, err
	}
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

func nested(outer, inner string) bool          { return sandbox.Nested(outer, inner) }
func lexicallyWithin(outer, inner string) bool { return sandbox.Within(outer, inner) }

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
	var powers any = base
	if c := o.Workbench.Commands; c != nil && c.LoopbackLocalOnly {
		powers = struct {
			Base              any
			LoopbackLocalOnly bool
		}{base, true}
	}
	if c := o.Workbench.Commands; c != nil && c.LoopbackPorts != nil {
		powers = struct {
			Base          any
			LoopbackPorts []int
		}{powers, c.LoopbackPorts}
	}
	if !o.Workbench.Write {
		return powers
	}
	mode := o.Workbench.NewFileMode
	if mode == 0 {
		mode = 0600
	}
	return struct {
		Base        any
		NewFileMode uint32
	}{powers, uint32(mode.Perm())}
}

// workbenchDefinitions are the workbench tools the options switch on. Both
// the request's tools and the host's admitted tools come from here.
func workbenchDefinitions(o Options) []ToolDefinition {
	if o.Workbench == nil {
		return nil
	}
	var defs []ToolDefinition
	for _, d := range sandbox.Definitions(o.Workbench.Write) {
		defs = append(defs, ToolDefinition{Name: d.Name, Description: d.Description, Schema: d.Schema})
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
	ws   *workbenchHost
	next ToolHandler
}

func (h workbenchHandler) CallTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	var r sandbox.Result
	var err error
	switch call.Name {
	case workbenchWriteFile:
		r, err = h.ws.files.Write(ctx, call.Arguments)
	case workbenchEditFile:
		r, err = h.ws.files.Edit(ctx, call.Arguments)
	case workbenchReadFile:
		r, err = h.ws.files.Read(ctx, call.Arguments)
	case workbenchSearchFiles:
		r, err = h.ws.files.Search(ctx, call.Arguments)
	case workbenchListFiles:
		r, err = h.ws.files.List(ctx, call.Arguments)
	case workbenchRunCommand:
		return h.ws.runCommand(ctx, call.Arguments)
	default:
		return h.next.CallTool(ctx, call)
	}
	return ToolResult{Content: r.Content, IsError: r.IsError}, h.ws.fromSandbox(err)
}
