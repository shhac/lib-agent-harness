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
