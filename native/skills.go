package native

import (
	"os"
	"slices"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/skills"
)

// Skills in a native run. Each engine was checked against its installed CLI
// with a disposable home and a local provider that performs no inference:
//
//   - Claude Code 2.1.283 loads a plugin named by --plugin-dir whose skills/
//     entries are symbolic links to skill directories: the startup frame lists
//     "agent-harness-skills:<name>" among its skills and the model request
//     carries the description. A scripted provider response calling its Skill
//     tool, under the default and dontAsk permission modes, was answered with
//     the skill's SKILL.md. --disable-slash-commands removes every skill, the
//     plugin's included, so a run whose Args carry it, --safe-mode or --tools
//     cannot take native delivery.
//   - Codex finds skills only in $CODEX_HOME/skills, and a native run uses the
//     operator's own home, which the library never writes. Headless Grok
//     (grok 1.0.41 --single) has no --plugin-dir. Both are composed: an index
//     of the skills, with each SKILL.md's absolute path, is appended to the
//     run's instructions and the agent reads them with its own tools.
//
// Installed skills stay as each CLI loads them (GlobalSkillsDefault, and
// Include, which is the same thing here). No CLI offers a verified switch that
// removes installed skills and nothing else (Claude's --setting-sources= also
// drops the operator's hooks and permission rules), so Exclude is refused.
//
// Skill.Scripts gates the library's own script runner. An agent that reads a
// skill through its own tools runs its scripts, or not, under its own
// permission policy; providing a skill grants no permission.

// skillDelivery is how one run's provided skills reach the model.
type skillDelivery int

const (
	noSkills skillDelivery = iota
	pluginSkills
	instructionSkills
)

// Codes a RunError carries for a skill request that cannot be honoured. A
// provided skill that fails to load carries the loader's own fixed code
// (such as skill_manifest_missing) as a preflight failure.
const (
	CodeGlobalSkillsInvalid     = "global_skills_invalid"
	CodeSkillDeliveryInvalid    = "skill_delivery_invalid"
	CodeGlobalSkillsUnsupported = "global_skills_unsupported"
	CodeSkillsConflict          = "skills_conflict"
	CodeSkillsUnavailable       = "skills_unavailable"
)

// claudeSkillSwitches are the Args flags that remove skills or the Skill
// tool: native delivery and installed skills cannot survive them.
var claudeSkillSwitches = []string{"disable-slash-commands", "safe-mode", "tools"}

// claudeSettingSwitches drop the user and project settings installed skills
// come from.
var claudeSettingSwitches = []string{"setting-sources", "bare"}

func planSkills(c Config) (skillDelivery, []skills.Skill, *RunError) {
	engine, set := c.Provider.Engine, c.Skills
	switch {
	case !set.Global.Known():
		return noSkills, nil, capabilityError(engine, CodeGlobalSkillsInvalid)
	case !set.Delivery.Known():
		return noSkills, nil, capabilityError(engine, CodeSkillDeliveryInvalid)
	case set.Global == harness.GlobalSkillsExclude:
		return noSkills, nil, capabilityError(engine, CodeGlobalSkillsUnsupported)
	case set.Global == harness.GlobalSkillsInclude && engine == harness.Claude && (argsCarry(c.Args, claudeSkillSwitches) || argsCarry(c.Args, claudeSettingSwitches)):
		return noSkills, nil, capabilityError(engine, CodeSkillsConflict)
	}
	if len(set.Provided) == 0 {
		return noSkills, nil, nil
	}
	loaded, err := skills.Load(set.Provided)
	if err != nil {
		return noSkills, nil, &RunError{Engine: engine, Family: harness.FailurePreflight, Code: skills.Code(err), cause: err}
	}
	if engine != harness.Claude || set.Delivery == harness.SkillDeliveryComposed {
		return instructionSkills, loaded, nil
	}
	if argsCarry(c.Args, claudeSkillSwitches) {
		return noSkills, nil, capabilityError(engine, CodeSkillsConflict)
	}
	return pluginSkills, loaded, nil
}

// applySkills delivers a planned skill set: a private plugin directory for
// Claude, or an index appended to the run's instructions. The returned
// function removes anything it wrote.
func applySkills(c Config, r Request, delivery skillDelivery, loaded []skills.Skill) (Config, Request, func(), error) {
	switch delivery {
	case pluginSkills:
		dir, err := privateDir(r.WorkDir, "skills-")
		if err != nil {
			return c, r, func() {}, &RunError{Engine: c.Provider.Engine, Family: harness.FailurePreflight, Code: CodeSkillsUnavailable, cause: err}
		}
		remove := func() { _ = os.RemoveAll(dir) }
		if err = skills.WritePlugin(dir, loaded); err != nil {
			remove()
			return c, r, func() {}, &RunError{Engine: c.Provider.Engine, Family: harness.FailurePreflight, Code: CodeSkillsUnavailable, cause: err}
		}
		c.Args = append(slices.Clone(c.Args), "--plugin-dir", dir)
		return c, r, remove, nil
	case instructionSkills:
		r.AppendInstructions = withIndex(r.AppendInstructions, skills.PathIndex(loaded))
		if c.Provider.Engine == harness.Claude {
			// Claude's file tools read outside the working directories only
			// with permission; the skill directories are added as working
			// directories, which grants reads there and nothing else.
			args := slices.Clone(c.Args)
			for _, skill := range loaded {
				args = append(args, "--add-dir", skill.Root())
			}
			c.Args = args
		}
	}
	return c, r, func() {}, nil
}

func withIndex(instructions, index string) string {
	if instructions == "" {
		return index
	}
	return instructions + "\n\n" + index
}

// argsCarry reports whether args set any of the named long flags.
func argsCarry(args []string, names []string) bool {
	for _, arg := range args {
		name, ok := strings.CutPrefix(arg, "--")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, "=")
		if slices.Contains(names, name) {
			return true
		}
	}
	return false
}
