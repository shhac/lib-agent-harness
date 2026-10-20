package harness

import (
	"context"

	"time"
)

// Skills an application relies on travel with its tools: a SKILL.md and the
// files it references. An engine's globally installed skills may stay in use,
// but wherever the library does not load them, the caller names exactly which
// skills are available for one invocation. A request that cannot be honoured
// as asked is refused before any work starts; nothing is silently dropped or
// widened.

// Skill is one caller-provided skill directory.
type Skill struct {
	// Name is the skill's name as its SKILL.md front matter states it:
	// lowercase letters, digits and hyphens, at most 64 characters. A
	// directory naming a different skill is refused.
	Name string
	// Dir is an absolute directory holding SKILL.md and the files it
	// references. Nothing outside it is ever read on the skill's behalf.
	Dir string
	// Scripts permits the skill's scripts to be run. Without it a request to
	// run one is refused; with it, each run still happens only when the caller
	// answers that request, which is the caller's authorization.
	Scripts bool
}

// GlobalSkills says whether the harness's own installed skills take part.
type GlobalSkills string

const (
	// GlobalSkillsDefault keeps the operation's own rule: whatever that mode
	// does with installed skills when the caller says nothing.
	GlobalSkillsDefault GlobalSkills = ""
	// GlobalSkillsInclude asks for the installed skills too. Where a mode
	// cannot load them, the request is refused rather than run without them.
	GlobalSkillsInclude GlobalSkills = "include"
	// GlobalSkillsExclude asks that no installed skill be available.
	GlobalSkillsExclude GlobalSkills = "exclude"
)

// Known reports whether g is one of the values above.
func (g GlobalSkills) Known() bool {
	switch g {
	case GlobalSkillsDefault, GlobalSkillsInclude, GlobalSkillsExclude:
		return true
	}
	return false
}

// SkillDelivery says how provided skills reach the model.
type SkillDelivery string

const (
	// SkillDeliveryAuto uses the harness's own skill mechanism where the
	// library has verified it, and composes skills otherwise.
	SkillDeliveryAuto SkillDelivery = ""
	// SkillDeliveryComposed always uses the library's composition: an index of
	// the provided skills and the library's skill tools, whatever the harness
	// could do itself.
	SkillDeliveryComposed SkillDelivery = "composed"
)

// Known reports whether d is one of the values above.
func (d SkillDelivery) Known() bool {
	return d == SkillDeliveryAuto || d == SkillDeliveryComposed
}

// Skills is what a caller makes available for one invocation. Support
// reports, per engine and operation, whether Provided skills can be made
// available (the Skills feature: native or composed) and whether installed
// skills can be included (the GlobalSkills feature). Excluding installed
// skills is accepted wherever that is already the operation's behaviour.
type Skills struct {
	Global   GlobalSkills
	Delivery SkillDelivery
	Provided []Skill
}

// SkillRunOptions says how a provided skill's scripts run when a caller
// answers a request to run one: completion.AnswerSkillCalls, a restricted
// session's hosted run_skill_script, or a handler from session.SkillTools.
type SkillRunOptions struct {
	// WorkDir is the scripts' working directory. It is required for scripts:
	// without it (or Exec) every script call is refused, and setting it is the
	// caller's standing authorization for a permitting skill's scripts to run
	// when the model asks.
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
