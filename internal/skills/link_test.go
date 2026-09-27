package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

func loadOne(t *testing.T, scripts bool) Skill {
	t.Helper()
	dir := writeSkill(t, "---\nname: demo\ndescription: Does a thing.\n---\nbody\n")
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "refs", "a.md"), []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load([]harness.Skill{{Name: "demo", Dir: dir, Scripts: scripts}})
	if err != nil {
		t.Fatal(err)
	}
	return loaded[0]
}

func TestWritePluginLinksEachSkill(t *testing.T) {
	skill := loadOne(t, false)
	dir := filepath.Join(t.TempDir(), "plugin")
	if err := WritePlugin(dir, []Skill{skill}); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil || !strings.Contains(string(manifest), `"name":"`+PluginName+`"`) {
		t.Fatalf("%q %v", manifest, err)
	}
	if text, err := os.ReadFile(filepath.Join(dir, "skills", "demo", "refs", "a.md")); err != nil || string(text) != "reference" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestIndexesNameEachSkill(t *testing.T) {
	skill := loadOne(t, true)
	path := PathIndex([]Skill{skill})
	for _, want := range []string{"- demo: Does a thing.", "SKILL.md: " + filepath.Join(skill.Root(), Manifest), "scripts may be run from " + skill.Root()} {
		if !strings.Contains(path, want) {
			t.Fatalf("path index lacks %q:\n%s", want, path)
		}
	}
	tool := ToolIndex([]Skill{skill})
	if !strings.Contains(tool, "- demo (scripts): Does a thing.") || strings.Contains(tool, skill.Root()) {
		t.Fatalf("tool index:\n%s", tool)
	}
}
