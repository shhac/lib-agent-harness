package harness

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
