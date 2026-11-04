//go:build darwin || linux

package sandboxbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDirs(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	home := filepath.Join(root, "home", "owner")
	tools := filepath.Join(root, "opt", "tools")
	inside := filepath.Join(home, "project")
	for _, dir := range []string{home, tools, inside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "to-home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	got, refusal := ReadDirs([]string{tools, inside})
	if refusal != "" || len(got) != 2 || got[0] != tools || got[1] != inside {
		t.Fatalf("ordinary directories: %q %q", got, refusal)
	}
	for name, dir := range map[string]string{
		"home":            home,
		"home's parent":   filepath.Dir(home),
		"filesystem root": "/",
		"link to home":    link,
	} {
		if _, refusal := ReadDirs([]string{tools, dir}); !strings.Contains(refusal, "would reopen the home directory") {
			t.Errorf("%s: %q", name, refusal)
		}
	}
	for name, dir := range map[string]string{
		"relative":       "opt/tools",
		"trailing slash": tools + "/",
		"dot-dot":        filepath.Join(tools, "..", "tools") + "/../tools",
	} {
		if _, refusal := ReadDirs([]string{dir}); !strings.Contains(refusal, "must be a clean absolute path") {
			t.Errorf("%s: %q", name, refusal)
		}
	}
}

func TestReadDirsRejectMissingPrivateAndPatternRoots(t *testing.T) {
	root := t.TempDir()
	for name, rel := range map[string]string{"private": ".ssh/keys", "pattern": "reference[1]", "git": ".git/objects", "config": ".config/tokens"} {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if _, refusal := ReadDirs([]string{dir}); refusal == "" {
			t.Errorf("accepted %s root", name)
		}
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(root, "missing"), file} {
		if _, refusal := ReadDirs([]string{dir}); refusal == "" {
			t.Errorf("accepted nonexistent/non-directory root %s", dir)
		}
	}
	for _, dir := range []string{"/tmp", "/usr", "/etc"} {
		if _, err := os.Stat(dir); err == nil {
			if _, refusal := ReadDirs([]string{dir}); refusal == "" {
				t.Errorf("accepted broad system root %s", dir)
			}
		}
	}
	private := filepath.Join(root, ".ssh", "keys")
	link := filepath.Join(root, "alias")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, refusal := ReadDirs([]string{link}); refusal == "" {
		t.Fatal("accepted alias to private root")
	}
}

func TestReadDirsKeepExplicitInstructionSkillLeaves(t *testing.T) {
	root := t.TempDir()
	for _, config := range []string{".claude", ".codex"} {
		leaf := filepath.Join(root, config, "skills", "demo")
		if err := os.MkdirAll(leaf, 0700); err != nil {
			t.Fatal(err)
		}
		if _, refusal := ReadDirs([]string{leaf}); refusal != "" {
			t.Fatalf("explicit skill leaf refused: %s", refusal)
		}
		if _, refusal := ReadDirs([]string{filepath.Join(root, config)}); refusal == "" {
			t.Fatal("native credential root admitted")
		}
	}
}
