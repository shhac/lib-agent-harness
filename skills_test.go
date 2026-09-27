package harness

import (
	"runtime"
	"testing"
)

func TestSkillChoicesAreAClosedVocabulary(t *testing.T) {
	for _, g := range []GlobalSkills{GlobalSkillsDefault, GlobalSkillsInclude, GlobalSkillsExclude} {
		if !g.Known() {
			t.Errorf("%q unknown", g)
		}
	}
	for _, d := range []SkillDelivery{SkillDeliveryAuto, SkillDeliveryComposed} {
		if !d.Known() {
			t.Errorf("%q unknown", d)
		}
	}
	if GlobalSkills("all").Known() || SkillDelivery("native").Known() {
		t.Error("an unlisted choice was accepted")
	}
}

func TestCompletionComposesSkillsAndNeverIncludesInstalledOnes(t *testing.T) {
	for _, e := range Engines() {
		if runtime.GOOS == "windows" && e == Grok {
			continue
		}
		if c := Support(e, Complete, ProvidedSkills); c.Availability != Composed || c.Reason == "" {
			t.Errorf("%s: %+v", e, c)
		}
		if c := Support(e, Complete, IncludeGlobalSkills); c.Availability != Unsupported || c.Reason == "" {
			t.Errorf("%s: %+v", e, c)
		}
	}
	for _, op := range []Operation{Models, Account} {
		if Support(Codex, op, ProvidedSkills).Usable() {
			t.Errorf("%s has no agent to use skills", op)
		}
	}
}
