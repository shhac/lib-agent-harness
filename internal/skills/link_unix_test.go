//go:build !windows

package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// Where a link cannot be made, the skill is copied: its directories and
// regular files, never anything a link inside it points at.
func TestCopyTreeKeepsOnlyTheSkillsOwnFiles(t *testing.T) {
	skill := loadOne(t, false)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(skill.Root(), "refs", "leak")); err != nil {
		t.Skip("links unavailable")
	}
	target := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(skill.Root(), target); err != nil {
		t.Fatal(err)
	}
	if text, err := os.ReadFile(filepath.Join(target, "refs", "a.md")); err != nil || string(text) != "reference" {
		t.Fatalf("%q %v", text, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "refs", "leak")); !os.IsNotExist(err) {
		t.Fatal("a link inside the skill was followed")
	}
}

func TestSyncLinksLeavesTheHarnessesOwnEntries(t *testing.T) {
	skill := loadOne(t, false)
	home := t.TempDir()
	skillsDir := filepath.Join(home, "skills")
	if err := SyncLinks(skillsDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(skillsDir); !os.IsNotExist(err) {
		t.Fatal("an empty set created the directory")
	}
	if err := os.MkdirAll(filepath.Join(skillsDir, ".system"), 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := SyncLinks(skillsDir, []Skill{skill}); err != nil {
			t.Fatal(err)
		}
	}
	if target, err := os.Readlink(filepath.Join(skillsDir, "demo")); err != nil || target != skill.Root() {
		t.Fatalf("%q %v", target, err)
	}
	if err := SyncLinks(skillsDir, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(skillsDir)
	if len(entries) != 1 || entries[0].Name() != ".system" {
		t.Fatalf("entries %v", entries)
	}
}
