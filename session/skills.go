package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/skills"
)

// Skills in a session. Every mechanism below was checked against the
// installed CLI (Claude Code 2.1.283, codex-cli 0.156.1, grok 1.0.41) with a
// disposable home and a local provider that performs no inference.
//
// Provided skills reach the model in one of three ways:
//
//   - Natively, where the harness loads them itself. An ordinary Claude
//     session loads a private plugin named by --plugin-dir (its skills are
//     listed as "agent-harness-skills:<name>", and a scripted Skill call under
//     dontAsk loaded the SKILL.md); it needs the Skill tool, so a
//     Policy.ClaudeTools without "Skill" takes the composed path instead. A
//     Grok session loads the same plugin through `grok agent --plugin-dir`
//     (the skill joins available_commands and the model request). A sandboxed
//     Codex session finds links in its private RuntimeHome's skills/
//     directory, which skills/list and the model request both carry under the
//     sandbox's own overrides; the operator's own Codex home is never written.
//   - As instructions: an index of the skills with each SKILL.md's absolute
//     path is appended to the session's instructions and the agent reads them
//     with its own tools. This is how an ordinary Codex session gets them, and
//     a sandboxed Claude session, whose --disable-slash-commands was observed
//     to remove every skill, a plugin's included; there the skill directories
//     are added to the sandbox's readable paths, as Sandbox.Read directories
//     are. An ordinary Claude session reading them this way gets each skill
//     directory as an --add-dir, without which dontAsk refused the read.
//   - As hosted tools: a restricted session has no native tool that could read
//     a file, so the library's load_skill (and run_skill_script, for skills
//     that permit scripts) are served beside the caller's tools, through the
//     same channel and under the same launch proof. The model sees an index of
//     them in its instructions.
//
// SkillDeliveryComposed takes the instructions path, or the hosted one in a
// restricted session, whatever the harness could do itself.
//
// Installed skills (Skills.Global) are left as each mode loads them by
// default. Include is honoured where that default already loads them: an
// ordinary session (for Claude, one whose tools keep "Skill"). Exclude is
// honoured where the default already loads none: a restricted session, which
// has no native tool that could use one, and a sandboxed Claude session. No
// CLI offers a verified switch that removes installed skills and nothing else
// (Claude's --setting-sources= also drops the operator's hooks and permission
// rules; Codex's skills.bundled.enabled=false leaves ~/.agents and repository
// skills), so every other request is refused.
//
// Scripts. With native or instruction delivery the agent runs a skill's
// scripts with its own shell, under the session's own policy and inside its
// sandbox when it has one; providing a skill grants no permission, and
// Skill.Scripts is stated in the index but cannot be enforced against an
// agent's own shell. With hosted delivery Skill.Scripts is enforced: only a
// permitting skill gets run_skill_script, and each call runs as SkillRun says.

// SkillRunOptions says how a restricted session's hosted run_skill_script, or
// a handler from SkillTools, runs a skill's scripts. It has the shape of
// completion.SkillRunOptions.
type SkillRunOptions struct {
	// WorkDir is the scripts' working directory. It is required for scripts:
	// setting it (or Exec) is the caller's standing authorization for the
	// hosted tool to run a permitting skill's scripts when the model asks.
	WorkDir string
	// Env is added to the scripts' environment, which otherwise holds only
	// the parent's PATH, HOME, TMPDIR, LANG and LC_* entries.
	Env []string
	// Timeout bounds each script; zero means one minute, and more than ten
	// minutes is refused.
	Timeout time.Duration
	// Exec, when set, runs each checked command instead of the library's
	// runner, for an application that runs scripts in its own container or
	// sandbox. Its error is never shown to the model.
	Exec func(context.Context, SkillCommand) (SkillOutput, error)
}

func (o SkillRunOptions) set() bool {
	return o.WorkDir != "" || o.Env != nil || o.Timeout != 0 || o.Exec != nil
}

// SkillCommand is one checked script invocation.
type SkillCommand struct {
	// Skill is the skill's name, and Dir its directory with every symbolic
	// link resolved.
	Skill string
	Dir   string
	// Script is the script's resolved absolute path, inside Dir.
	Script string
	// Args is the argument vector; no shell reads it.
	Args    []string
	WorkDir string
	// Env is the allowlisted environment with SkillRunOptions.Env applied.
	Env     []string
	Timeout time.Duration
}

// SkillOutput is what a script produced. Stdout and Stderr are each bounded
// to 64 KiB; the rest is discarded and marked truncated.
type SkillOutput struct {
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	Stdout          string `json:"stdout"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	Stderr          string `json:"stderr"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

// The skill tools' names, reserved in any tool host they are mounted in.
const (
	LoadSkillTool      = skills.LoadTool
	RunSkillScriptTool = skills.RunScriptTool
)

// Refusal codes for skill requests. A provided skill that fails to load is
// refused with the loader's own fixed code, such as skill_manifest_missing.
const (
	RefusedGlobalSkillsInvalid  = "global_skills_invalid"
	RefusedSkillDeliveryInvalid = "skill_delivery_invalid"
	// RefusedGlobalSkills: the mode cannot honour the Skills.Global request.
	RefusedGlobalSkills = "global_skills_unsupported"
	// RefusedSkills: the mode's tools cannot deliver the provided skills.
	RefusedSkills            = "skills_unsupported"
	RefusedSkillToolReserved = "skill_tool_name_reserved"
	RefusedSkillFiles        = "skill_files_unavailable"
)

type skillDelivery int

const (
	nativeSkills skillDelivery = iota + 1
	instructionSkills
	hostedSkills
	// requestSkills: an API session's requests carry completion's composed
	// skill index and tools, and the library answers the calls.
	requestSkills
)

// skillPlan is how one launch delivers its provided skills. pluginDir is set
// once the launch has written its plugin.
type skillPlan struct {
	delivery  skillDelivery
	loaded    []skills.Skill
	pluginDir string
}

// normalizeSkills checks the skill request against the session's mode and
// plans its delivery. It runs after the sandbox, policy and restriction are
// normalized, since it judges what those left the session.
func normalizeSkills(o Options) (Options, error) {
	set := o.Skills
	if !set.Global.Known() {
		return o, refuse(o, "skills", RefusedGlobalSkillsInvalid, "Skills.Global is not a known value")
	}
	if !set.Delivery.Known() {
		return o, refuse(o, "skills", RefusedSkillDeliveryInvalid, "Skills.Delivery is not a known value")
	}
	if err := globalSkillsProblem(o); err != nil {
		return o, err
	}
	if len(set.Provided) == 0 {
		if o.SkillRun.set() {
			return o, refuse(o, "skills", RefusedConflict, "SkillRun applies only to provided skills that permit scripts in a restricted session")
		}
		return o, nil
	}
	loaded, err := skills.Load(set.Provided)
	if err != nil {
		return o, refuse(o, "skills", skills.Code(err), "a provided skill could not be loaded")
	}
	o.Skills.Provided = slices.Clone(set.Provided)
	plan := &skillPlan{loaded: loaded}
	if plan.delivery, err = skillDeliveryFor(o); err != nil {
		return o, err
	}
	_, scripted := skills.Names(loaded)
	hostsScripts := plan.delivery == hostedSkills && len(scripted) > 0
	switch {
	case o.SkillRun.set() && !hostsScripts:
		return o, refuse(o, "skills", RefusedConflict, "SkillRun applies only to provided skills that permit scripts in a restricted session")
	case hostsScripts && o.SkillRun.WorkDir == "":
		return o, refuse(o, "skills", skills.CodeWorkDirRequired, "a restricted session hosting skill scripts requires SkillRun.WorkDir")
	}
	o.SkillRun.Env = slices.Clone(o.SkillRun.Env)
	if plan.delivery == instructionSkills && o.Sandbox != nil && o.Provider.Engine == harness.Claude {
		if _, problem := sandboxReadDirs(skillRoots(loaded)); problem != "" {
			return o, refuse(o, "skills", RefusedSandboxRead, "a provided skill directory would reopen the home directory to the sandbox")
		}
	}
	if plan.delivery == hostedSkills {
		if o, err = mountSkillTools(o, loaded); err != nil {
			return o, err
		}
	}
	o.skills = plan
	return o, nil
}

// globalSkillsProblem refuses an installed-skills request the mode cannot
// honour as asked.
func globalSkillsProblem(o Options) error {
	engine := o.Provider.Engine
	switch o.Skills.Global {
	case harness.GlobalSkillsInclude:
		switch {
		case o.Restriction != nil:
			return refuse(o, "skills", RefusedGlobalSkills, "a restricted session has no native tool that could use an installed skill")
		case o.Sandbox != nil && engine == harness.Claude:
			return refuse(o, "skills", RefusedGlobalSkills, "a sandboxed Claude session loads no installed skills")
		case o.Sandbox != nil:
			return refuse(o, "skills", RefusedGlobalSkills, "a sandboxed Codex session runs in a private home that does not share the operator's installed skills")
		case engine == harness.Claude && !claudeTool(o, "Skill"):
			return refuse(o, "skills", RefusedGlobalSkills, "installed skills need the Skill tool; add it to Policy.ClaudeTools")
		}
	case harness.GlobalSkillsExclude:
		if o.Restriction != nil || (o.Sandbox != nil && engine == harness.Claude) {
			return nil
		}
		return refuse(o, "skills", RefusedGlobalSkills, "no verified switch removes this engine's installed skills in this mode")
	}
	return nil
}

func skillDeliveryFor(o Options) (skillDelivery, error) {
	engine := o.Provider.Engine
	if o.Restriction != nil {
		return hostedSkills, nil
	}
	composed := o.Skills.Delivery == harness.SkillDeliveryComposed
	switch engine {
	case harness.Grok:
		if composed {
			return instructionSkills, nil
		}
		return nativeSkills, nil
	case harness.Codex:
		if o.Sandbox != nil && !composed {
			return nativeSkills, nil
		}
		return instructionSkills, nil
	}
	if o.Sandbox == nil && !composed && claudeTool(o, "Skill") {
		return nativeSkills, nil
	}
	if !claudeTool(o, "Read") {
		return 0, refuse(o, "skills", RefusedSkills, "a Claude session reads provided skills with its Read tool, or loads them with its Skill tool; Policy.ClaudeTools has neither")
	}
	return instructionSkills, nil
}

// claudeTool reports whether a Claude session's policy keeps a native tool.
// Nil ClaudeTools keeps every one.
func claudeTool(o Options, name string) bool {
	return o.Policy.ClaudeTools == nil || slices.Contains(o.Policy.ClaudeTools, name)
}

func skillRoots(loaded []skills.Skill) []string {
	roots := make([]string, 0, len(loaded))
	for _, skill := range loaded {
		roots = append(roots, skill.Root())
	}
	return roots
}

// mountSkillTools adds the skill tools to a restricted session's tool host.
// The host is the session's own copy, so the caller's value is untouched.
func mountSkillTools(o Options, loaded []skills.Skill) (Options, error) {
	host := o.Restriction.Tools
	for _, tool := range host.Tools {
		if skills.IsTool(tool.Name) {
			return o, refuse(o, "tools", RefusedSkillToolReserved, "load_skill and run_skill_script are reserved for the library's skill tools")
		}
	}
	host.Tools = append(slices.Clone(host.Tools), freezeTools(skillToolDefinitions(loaded))...)
	host.Handler = skillHandler{loaded: loaded, opts: runOptions(o.SkillRun), next: host.Handler}
	if err := host.validate(); err != nil {
		return o, toolHostRefusal(o, err)
	}
	restriction := *o.Restriction
	restriction.Tools = host
	o.Restriction = &restriction
	return o, nil
}

func skillToolDefinitions(loaded []skills.Skill) []ToolDefinition {
	names, scripted := skills.Names(loaded)
	tools := []ToolDefinition{{Name: skills.LoadTool, Description: skills.LoadToolDescription, Schema: skills.LoadToolSchema(names)}}
	if len(scripted) > 0 {
		tools = append(tools, ToolDefinition{Name: skills.RunScriptTool, Description: skills.RunScriptToolDescription, Schema: skills.RunScriptToolSchema(scripted)})
	}
	return tools
}

// SkillTools returns the library's skill tools for set.Provided and a handler
// that answers them, for a caller that hosts them itself, such as beside its
// own tools in a sandboxed session. A restricted session with Options.Skills
// mounts the same tools automatically. load_skill reads a skill's files,
// contained and bounded; run_skill_script, offered only when a provided skill
// permits scripts, runs one as opts says, and is refused for every call
// without opts.WorkDir. The handler answers a failure as an error result
// naming a fixed code, and any other tool name with ErrToolUnknown.
func SkillTools(set harness.Skills, opts SkillRunOptions) ([]ToolDefinition, ToolHandler, error) {
	if len(set.Provided) == 0 {
		return nil, nil, &UnsupportedError{Operation: "skills", Code: RefusedNotConfigured, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "no provided skills"}}
	}
	loaded, err := skills.Load(set.Provided)
	if err != nil {
		return nil, nil, &UnsupportedError{Operation: "skills", Code: skills.Code(err), Capability: harness.Capability{Availability: harness.Unsupported, Reason: "a provided skill could not be loaded"}}
	}
	opts.Env = slices.Clone(opts.Env)
	return skillToolDefinitions(loaded), skillHandler{loaded: loaded, opts: runOptions(opts)}, nil
}

type skillHandler struct {
	loaded []skills.Skill
	opts   skills.RunOptions
	next   ToolHandler
}

func (h skillHandler) CallTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	if !skills.IsTool(call.Name) {
		if h.next == nil {
			return ToolResult{}, ErrToolUnknown
		}
		return h.next.CallTool(ctx, call)
	}
	content, failed := skills.Answer(ctx, h.loaded, call.Name, call.Arguments, h.opts)
	return ToolResult{Content: content, IsError: failed}, nil
}

func runOptions(o SkillRunOptions) skills.RunOptions {
	run := skills.RunOptions{WorkDir: o.WorkDir, Env: o.Env, Timeout: o.Timeout}
	if o.Exec != nil {
		run.Exec = func(ctx context.Context, c skills.ExecCommand) (skills.ExecOutput, error) {
			out, err := o.Exec(ctx, SkillCommand(c))
			return skills.ExecOutput(out), err
		}
	}
	return run
}

// effectiveInstructions are the instructions a launch sends: the caller's,
// with the skill index appended where skills are delivered as instructions
// or hosted tools. A Ref digests the caller's own, never these, so editing a
// skill's description does not orphan a stored session.
func effectiveInstructions(o Options) Instructions {
	plan := o.skills
	if plan == nil || plan.delivery == nativeSkills {
		return o.Instructions
	}
	index := skills.PathIndex(plan.loaded)
	if plan.delivery == hostedSkills {
		index = skills.ToolIndex(plan.loaded)
	}
	in := o.Instructions
	if in.Mode == "" {
		in.Mode = Append
	}
	if in.Text != "" {
		index = in.Text + "\n\n" + index
	}
	in.Text = index
	return in
}

// claudeSkillArgs load the private plugin, or make the skill directories
// readable to an ordinary session's file tools. A sandboxed session's reads
// are opened in its sandbox settings instead.
func claudeSkillArgs(o Options) []string {
	plan := o.skills
	switch {
	case plan == nil:
		return nil
	case plan.delivery == nativeSkills:
		return []string{"--plugin-dir", plan.pluginDir}
	case plan.delivery == instructionSkills && o.Sandbox == nil:
		var args []string
		for _, root := range skillRoots(plan.loaded) {
			args = append(args, "--add-dir", root)
		}
		return args
	}
	return nil
}

// sandboxSkillReads are the skill directories a sandboxed Claude session's
// shell and Read tool may also read.
func sandboxSkillReads(o Options) []string {
	if o.skills == nil || o.skills.delivery != instructionSkills {
		return nil
	}
	return skillRoots(o.skills.loaded)
}

// prepareSkillFiles writes what a launch's native delivery reads: the private
// plugin for Claude and Grok, or the links in a Codex runtime home. The
// returned function removes a plugin once the session is over.
func prepareSkillFiles(o Options) (func(), error) {
	plan := o.skills
	if plan == nil || plan.delivery != nativeSkills || o.Provider.Engine == harness.Codex {
		return func() {}, nil
	}
	dir, err := skillPluginDir(o.WorkDir)
	if err != nil {
		return func() {}, refuse(o, "skills", RefusedSkillFiles, "no private directory outside the working directory is available for the provided skills")
	}
	remove := func() { _ = os.RemoveAll(dir) }
	if err = skills.WritePlugin(dir, plan.loaded); err != nil {
		remove()
		return func() {}, refuse(o, "skills", RefusedSkillFiles, "the provided skills could not be linked into a private plugin")
	}
	plan.pluginDir = dir
	return remove, nil
}

// syncRuntimeSkills leaves a Codex runtime home holding exactly this launch's
// natively delivered skills, removing any an earlier launch linked.
func syncRuntimeSkills(o Options) error {
	var loaded []skills.Skill
	if o.skills != nil && o.skills.delivery == nativeSkills {
		loaded = o.skills.loaded
	}
	if err := skills.SyncLinks(filepath.Join(o.RuntimeHome, "skills"), loaded); err != nil {
		return refuse(o, "skills", RefusedSkillFiles, "the provided skills could not be linked into the runtime home")
	}
	return nil
}

// skillPluginDir makes a private directory for one launch's plugin, outside
// the working directory.
func skillPluginDir(workDir string) (string, error) {
	var roots []string
	if cache, err := os.UserCacheDir(); err == nil {
		roots = append(roots, filepath.Join(cache, "lib-agent-harness", "session-skills"))
	}
	roots = append(roots, os.TempDir())
	workspace := workDir
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workspace = resolved
	}
	var lastErr error = os.ErrNotExist
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o700); err != nil {
			lastErr = err
			continue
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil && inside(workspace, resolved) {
			continue
		}
		dir, err := os.MkdirTemp(root, "skills-")
		if err != nil {
			lastErr = err
			continue
		}
		return dir, nil
	}
	return "", lastErr
}

func inside(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && (len(rel) < 3 || rel[:3] != ".."+string(filepath.Separator))
}

// skillsDigest is what a Ref records of the skill request: what changes the
// agent's surface (installed skills, delivery, and which skills with which
// permission), not where a skill lives or what its files say. Nil for a
// session without a skill request, whose digest must not move.
func skillsDigest(o Options) any {
	set := o.Skills
	if set.Global == harness.GlobalSkillsDefault && set.Delivery == harness.SkillDeliveryAuto && len(set.Provided) == 0 {
		return nil
	}
	type provided struct {
		Name    string
		Scripts bool `json:",omitempty"`
	}
	names := make([]provided, 0, len(set.Provided))
	for _, skill := range set.Provided {
		names = append(names, provided{skill.Name, skill.Scripts})
	}
	return struct {
		Global   harness.GlobalSkills  `json:",omitempty"`
		Delivery harness.SkillDelivery `json:",omitempty"`
		Provided []provided            `json:",omitempty"`
	}{set.Global, set.Delivery, names}
}
