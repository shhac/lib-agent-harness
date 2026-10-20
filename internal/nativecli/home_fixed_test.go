package nativecli

import (
	"path/filepath"
	"testing"
)

// A CLI whose home no variable moves accepts only its own folder, and each
// refusal keeps the engine-prefixed code callers match on.
func TestFixedHomeProblem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for name, c := range map[string]struct {
		home, want string
	}{
		"unset":        {"", ""},
		"native":       {filepath.Join(home, ".fx"), ""},
		"native dirty": {filepath.Join(home, ".fx", "..", ".fx") + "/", ""},
		"relative":     {".fx", "fx_home_invalid"},
		"another":      {filepath.Join(home, "other"), "fx_home_unsupported"},
	} {
		if got := FxHomeProblem(c.home); got != c.want {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := AntigravityHomeProblem(filepath.Join(home, ".fx")); got != "antigravity_home_unsupported" {
		t.Errorf("antigravity accepted fx's home: %q", got)
	}
	if got := AntigravityHomeProblem(filepath.Join(home, ".gemini")); got != "" {
		t.Errorf("antigravity refused its own home: %q", got)
	}
}
