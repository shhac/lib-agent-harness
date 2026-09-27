package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// The library's own skill tools, as the model sees them wherever skills are
// composed rather than loaded by the harness: a read-only load_skill, and
// run_skill_script for skills that permit scripts. Package completion
// proposes them; a restricted session hosts them. Their names, schemas and
// answers are the same everywhere.
const (
	LoadTool      = "load_skill"
	RunScriptTool = "run_skill_script"

	LoadToolDescription      = "Read one of the available skills: its SKILL.md by default, or a file it references, by path relative to the skill's directory. Read-only. The content arrives as this call's tool result."
	RunScriptToolDescription = "Ask the application to run a script inside a skill that permits scripts. The script is a path relative to the skill's directory; it runs directly, without a shell, with args as its argument list. The application decides whether it runs; the exit code and bounded output arrive as this call's tool result."
)

// IsTool reports whether name is one of the library's skill tools.
func IsTool(name string) bool { return name == LoadTool || name == RunScriptTool }

// Names lists the loaded skills' names, and those that permit scripts.
func Names(loaded []Skill) (all, scripted []string) {
	for _, skill := range loaded {
		all = append(all, skill.Name)
		if skill.Scripts {
			scripted = append(scripted, skill.Name)
		}
	}
	return all, scripted
}

// LoadToolSchema is load_skill's argument schema for these skill names.
func LoadToolSchema(names []string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill": map[string]any{"type": "string", "enum": append([]string{}, names...), "description": "The skill's name."},
			"file":  map[string]any{"type": "string", "description": "A path relative to the skill's directory. Defaults to SKILL.md."},
		},
		"required":             []string{"skill"},
		"additionalProperties": false,
	}
}

// RunScriptToolSchema is run_skill_script's argument schema for these skill
// names, which must all permit scripts.
func RunScriptToolSchema(names []string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill":  map[string]any{"type": "string", "enum": append([]string{}, names...), "description": "The skill's name."},
			"script": map[string]any{"type": "string", "description": "The script's path relative to the skill's directory."},
			"args":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The script's arguments."},
		},
		"required":             []string{"skill", "script"},
		"additionalProperties": false,
	}
}

// ToolIndex tells the model which skills the library's skill tools serve.
func ToolIndex(loaded []Skill) string {
	var b strings.Builder
	_, scripted := Names(loaded)
	b.WriteString("Skills are available through the " + LoadTool + " function. A skill is a SKILL.md of instructions plus files it references. Before relying on a skill, call " + LoadTool + " with its name to read its SKILL.md, then with file set to read a file it references.")
	if len(scripted) > 0 {
		b.WriteString(" Skills marked (scripts) may have their scripts run through the " + RunScriptTool + " function.")
	}
	b.WriteString("\n\nAvailable skills:")
	for _, skill := range loaded {
		b.WriteString("\n- " + skill.Name)
		if skill.Scripts {
			b.WriteString(" (scripts)")
		}
		b.WriteString(": " + skill.Description)
	}
	return b.String()
}

// PathIndex tells an agent with its own file tools where each skill lives,
// for delivery through its instructions. A skill that does not permit scripts
// says so, since the agent's own shell could otherwise run them.
func PathIndex(loaded []Skill) string {
	var b strings.Builder
	b.WriteString("Skills are available as files on disk. A skill is a SKILL.md of instructions plus files it references, by paths relative to the skill's directory. When a task matches a skill's description, read its SKILL.md in full with your file tools before acting, then read the files it references from the same directory.")
	b.WriteString("\n\nAvailable skills:")
	for _, skill := range loaded {
		b.WriteString("\n- " + skill.Name + ": " + skill.Description)
		b.WriteString("\n  SKILL.md: " + filepath.Join(skill.Root(), Manifest))
		if skill.Scripts {
			b.WriteString("\n  Its scripts may be run from " + skill.Root() + " with your own shell tool, under your usual permissions.")
		} else {
			b.WriteString("\n  Do not run its scripts.")
		}
	}
	return b.String()
}

// Call is a skill tool call's decoded arguments.
type Call struct {
	Skill  string   `json:"skill"`
	File   string   `json:"file"`
	Script string   `json:"script"`
	Args   []string `json:"args"`
}

// DecodeCall reads a skill call's arguments strictly: a JSON object with only
// that tool's fields, each of its type.
func DecodeCall(tool string, arguments []byte) (Call, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(arguments, &fields) != nil || fields == nil {
		return Call{}, false
	}
	allowed := map[string]bool{"skill": true}
	switch tool {
	case LoadTool:
		allowed["file"] = true
	case RunScriptTool:
		allowed["script"], allowed["args"] = true, true
		if fields["script"] == nil {
			return Call{}, false
		}
	default:
		return Call{}, false
	}
	for key, value := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Call{}, false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var decoded Call
	if decoder.Decode(&decoded) != nil || decoded.Skill == "" {
		return Call{}, false
	}
	if _, present := fields["file"]; present && decoded.File == "" {
		return Call{}, false
	}
	return decoded, true
}

// ExecCommand is one checked script invocation handed to a caller's runner.
type ExecCommand struct {
	Skill, Dir, Script string
	Args               []string
	WorkDir            string
	Env                []string
	Timeout            time.Duration
}

// ExecOutput is what a script produced, as a run_skill_script answer carries it.
type ExecOutput struct {
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	Stdout          string `json:"stdout"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	Stderr          string `json:"stderr"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

// RunOptions says how Answer runs scripts: in WorkDir, which is required,
// with Env added to the allowlisted environment, bounded by Timeout, and
// through Exec instead of Run when it is set.
type RunOptions struct {
	WorkDir string
	Env     []string
	Timeout time.Duration
	Exec    func(context.Context, ExecCommand) (ExecOutput, error)
}

// Answer answers one skill tool call. A load_skill answer is the file's text;
// a run_skill_script answer is ExecOutput as JSON. Anything that cannot be
// answered is a fixed error text naming a code, with failed set.
func Answer(ctx context.Context, loaded []Skill, tool string, arguments []byte, opts RunOptions) (content string, failed bool) {
	decoded, ok := DecodeCall(tool, arguments)
	if !ok {
		return ErrorText(tool, "invalid_arguments"), true
	}
	var skill Skill
	found := false
	for _, candidate := range loaded {
		if candidate.Name == decoded.Skill {
			skill, found = candidate, true
		}
	}
	if !found {
		return ErrorText(tool, "unknown_skill"), true
	}
	if tool == LoadTool {
		file := decoded.File
		if file == "" {
			file = Manifest
		}
		text, err := skill.Read(file)
		if err != nil {
			return ErrorText(tool, Code(err)), true
		}
		return text, false
	}
	return runScript(ctx, skill, decoded, opts)
}

// ErrorText is the fixed text a skill tool answers with when it cannot answer.
func ErrorText(tool, code string) string { return tool + " error: " + code }

func runScript(ctx context.Context, skill Skill, call Call, opts RunOptions) (string, bool) {
	command, err := Prepare(skill, call.Script, call.Args, opts.WorkDir, opts.Env, opts.Timeout)
	if err != nil {
		return ErrorText(RunScriptTool, Code(err)), true
	}
	if ctx.Err() != nil {
		return ErrorText(RunScriptTool, "cancelled"), true
	}
	var output ExecOutput
	if opts.Exec != nil {
		output, err = opts.Exec(ctx, ExecCommand{Skill: skill.Name, Dir: skill.Root(), Script: command.Script, Args: command.Args, WorkDir: command.WorkDir, Env: command.Env, Timeout: command.Timeout})
		if err != nil {
			return ErrorText(RunScriptTool, "script_failed"), true
		}
		output.Stdout, output.StdoutTruncated = bound(output.Stdout, output.StdoutTruncated)
		output.Stderr, output.StderrTruncated = bound(output.Stderr, output.StderrTruncated)
	} else {
		ran, err := Run(ctx, command)
		if err != nil {
			if code := Code(err); code != "" {
				return ErrorText(RunScriptTool, code), true
			}
			return ErrorText(RunScriptTool, "cancelled"), true
		}
		output = ExecOutput{ExitCode: ran.ExitCode, TimedOut: ran.TimedOut, Stdout: ran.Stdout, StdoutTruncated: ran.StdoutTruncated, Stderr: ran.Stderr, StderrTruncated: ran.StderrTruncated}
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return ErrorText(RunScriptTool, "script_failed"), true
	}
	return string(encoded), false
}

func bound(text string, truncated bool) (string, bool) {
	if len(text) <= MaxOutputBytes {
		return text, truncated
	}
	return text[:MaxOutputBytes], true
}
