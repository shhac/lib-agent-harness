package completion

import (
	"context"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/skills"
)

// Skills are composed for every engine: constrained completion has no native
// tools, so no harness loads skills here. With Config.Skills.Provided set, the
// model sees an index of the provided skills (name and description from each
// SKILL.md) in the system context, and the library's own skill tools beside
// the caller's: load_skill always, and run_skill_script when a provided skill
// permits scripts. For a CLI engine both travel in the one rendered payload
// that already carries messages and available_tools, so the proven
// instruction surface is unchanged; an OpenAI-compatible request carries them
// as function tools and in its system message.
//
// A skill call is a proposal like any other. It stays in Message.ToolCalls,
// in the order the model made it, so the assistant message can be appended to
// history as it is and every later tool message has its matching call, which
// Chat Completions requires. Result.SkillCalls lists the skill calls again,
// and Result.ApplicationCalls the rest. AnswerSkillCalls produces the tool
// messages for the skill calls; the caller answers its own.

const (
	// LoadSkillTool reads a provided skill's SKILL.md or a file inside it.
	LoadSkillTool = "load_skill"
	// RunSkillScriptTool runs a script inside a provided skill that permits
	// scripts, when the caller answers the call.
	RunSkillScriptTool = "run_skill_script"
)

func skillTool(name string) bool { return name == LoadSkillTool || name == RunSkillScriptTool }

// composeSkills checks the caller's skill request and returns the messages
// and tools the model sees. Without provided skills nothing changes.
func composeSkills(engine harness.Engine, set harness.Skills, messages []Message, tools []Tool) ([]Message, []Tool, error) {
	if !set.Global.Known() {
		return nil, nil, preflightFailure(engine, "global_skills_invalid")
	}
	if set.Global == harness.GlobalSkillsInclude && !harness.Support(engine, harness.Complete, harness.IncludeGlobalSkills).Usable() {
		return nil, nil, preflightFailure(engine, "global_skills_unsupported")
	}
	if !set.Delivery.Known() {
		return nil, nil, preflightFailure(engine, "skill_delivery_invalid")
	}
	if len(set.Provided) == 0 {
		return messages, tools, nil
	}
	if !harness.Support(engine, harness.Complete, harness.ProvidedSkills).Usable() {
		return nil, nil, preflightFailure(engine, "skills_unsupported")
	}
	for _, tool := range tools {
		if skillTool(tool.Function.Name) {
			return nil, nil, preflightFailure(engine, "skill_tool_name_reserved")
		}
	}
	loaded, err := skills.Load(set.Provided)
	if err != nil {
		return nil, nil, preflightFailure(engine, skills.Code(err))
	}
	var names, scripted []string
	for _, skill := range loaded {
		names = append(names, skill.Name)
		if skill.Scripts {
			scripted = append(scripted, skill.Name)
		}
	}
	composed := append(append([]Tool{}, tools...), loadSkillDefinition(names))
	if len(scripted) > 0 {
		composed = append(composed, runSkillScriptDefinition(scripted))
	}
	return withSkillIndex(messages, skillIndex(loaded, len(scripted) > 0)), composed, nil
}

func loadSkillDefinition(names []string) Tool {
	return Tool{Type: "function", Function: Function{
		Name:        LoadSkillTool,
		Description: "Read one of the available skills: its SKILL.md by default, or a file it references, by path relative to the skill's directory. Read-only. The content arrives as this call's tool result.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill": map[string]any{"type": "string", "enum": names, "description": "The skill's name."},
				"file":  map[string]any{"type": "string", "description": "A path relative to the skill's directory. Defaults to SKILL.md."},
			},
			"required":             []string{"skill"},
			"additionalProperties": false,
		},
	}}
}

func runSkillScriptDefinition(names []string) Tool {
	return Tool{Type: "function", Function: Function{
		Name:        RunSkillScriptTool,
		Description: "Ask the application to run a script inside a skill that permits scripts. The script is a path relative to the skill's directory; it runs directly, without a shell, with args as its argument list. The application decides whether it runs; the exit code and bounded output arrive as this call's tool result.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill":  map[string]any{"type": "string", "enum": names, "description": "The skill's name."},
				"script": map[string]any{"type": "string", "description": "The script's path relative to the skill's directory."},
				"args":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The script's arguments."},
			},
			"required":             []string{"skill", "script"},
			"additionalProperties": false,
		},
	}}
}

func skillIndex(loaded []skills.Skill, scripts bool) string {
	var b strings.Builder
	b.WriteString("Skills are available through the " + LoadSkillTool + " function. A skill is a SKILL.md of instructions plus files it references. Before relying on a skill, call " + LoadSkillTool + " with its name to read its SKILL.md, then with file set to read a file it references.")
	if scripts {
		b.WriteString(" Skills marked (scripts) may have their scripts run through the " + RunSkillScriptTool + " function.")
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

// withSkillIndex puts the index in the system context without changing the
// caller's slice: appended to a leading system message, or as a new one,
// since some compatible endpoints accept only one leading system message.
func withSkillIndex(messages []Message, index string) []Message {
	out := append([]Message{}, messages...)
	if len(out) > 0 && out[0].Role == "system" {
		if out[0].Content != "" {
			index = out[0].Content + "\n\n" + index
		}
		out[0].Content = index
		return out
	}
	return append([]Message{{Role: "system", Content: index}}, out...)
}

// separateSkillCalls lists a reply's skill calls in Result.SkillCalls, after
// checking their arguments' shape. Which skill and path they name is checked
// when they are answered, so a mistaken one becomes an error the model can
// read rather than a failed request.
func separateSkillCalls(engine harness.Engine, result Result) (Result, error) {
	for _, call := range result.Message.ToolCalls {
		if !skillTool(call.Function.Name) {
			continue
		}
		if _, ok := skills.DecodeCall(call.Function.Name, []byte(call.Function.Arguments)); !ok {
			return Result{Usage: result.Usage, Cost: result.Cost, ContextWindow: result.ContextWindow},
				&RequestError{Cause: harness.CauseUnknown, Engine: engine, Phase: PhaseResponse, Code: "invalid_skill_call"}
		}
		result.SkillCalls = append(result.SkillCalls, call)
	}
	return result, nil
}

// ApplicationCalls is Message.ToolCalls without the skill calls: the
// proposals the caller authorizes and executes itself.
func (r Result) ApplicationCalls() []ToolCall {
	skill := map[string]bool{}
	for _, call := range r.SkillCalls {
		skill[call.ID] = true
	}
	var calls []ToolCall
	for _, call := range r.Message.ToolCalls {
		if !skill[call.ID] {
			calls = append(calls, call)
		}
	}
	return calls
}

// SkillRunOptions says how AnswerSkillCalls runs a skill's scripts.
type SkillRunOptions struct {
	// WorkDir is the scripts' working directory. It is required: without it
	// every script call is refused.
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

// AnswerSkillCalls answers the skill calls among calls, one role "tool"
// message per call, in order; calls to other tools are skipped, so
// Message.ToolCalls may be passed whole. A load_skill answer is the file's
// text. A run_skill_script answer is a JSON object with the exit code and
// bounded output. Anything that cannot be answered, such as an unknown skill,
// a path outside the skill, or a skill that does not permit scripts, is
// answered with a fixed error text naming a code, so the history stays valid
// and the model can recover.
//
// Answering is the caller's authorization: the library runs a script only
// when a run_skill_script call is passed here, and one at a time. Scripts
// are refused on Windows unless opts.Exec runs them.
func AnswerSkillCalls(ctx context.Context, calls []ToolCall, set harness.Skills, opts SkillRunOptions) []Message {
	var answers []Message
	loaded, err := skills.Load(set.Provided)
	for _, call := range calls {
		if !skillTool(call.Function.Name) {
			continue
		}
		content := skills.ErrorText(call.Function.Name, "skills_unavailable")
		if err == nil {
			content, _ = skills.Answer(ctx, loaded, call.Function.Name, []byte(call.Function.Arguments), runOptions(opts))
		}
		answers = append(answers, Message{Role: "tool", ToolCallID: call.ID, Content: content})
	}
	return answers
}

// runOptions carries SkillRunOptions into the shared skill runner, adapting
// only the caller's Exec at its boundary.
func runOptions(opts SkillRunOptions) skills.RunOptions {
	run := skills.RunOptions{WorkDir: opts.WorkDir, Env: opts.Env, Timeout: opts.Timeout}
	if opts.Exec == nil {
		return run
	}
	run.Exec = func(ctx context.Context, c skills.ExecCommand) (skills.ExecOutput, error) {
		out, err := opts.Exec(ctx, SkillCommand{Skill: c.Skill, Dir: c.Dir, Script: c.Script, Args: c.Args, WorkDir: c.WorkDir, Env: c.Env, Timeout: c.Timeout})
		return skills.ExecOutput(out), err
	}
	return run
}
