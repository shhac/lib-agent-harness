package nativecli

import (
	"os"
	"path/filepath"
	"strings"
)

// FxEnvironment is the operating environment an fx launch runs with.
//
// fx keeps its profile in ~/.fx, which it derives from the user's home
// directory, and it has no variable that moves it: no FX_HOME, and no
// XDG_CONFIG_HOME either. A launch therefore runs with the operator's own
// home, and a home that names anything else is refused rather than silently
// ignored, because a caller who selected a home expects that home to be the
// one the CLI reads.
//
// The environment is the allowlisted operating one: fx resolves its login
// natively, through the macOS keychain or its own settings file, and no
// ambient FX_ variable, API key or model override reaches it.
func FxEnvironment(home string) ([]string, string) {
	if code := FxHomeProblem(home); code != "" {
		return nil, code
	}
	return Native(), ""
}

// FxHomeProblem accepts the native fx profile and nothing else: the home
// directory fx itself resolves, and no other.
func FxHomeProblem(home string) string {
	if home == "" {
		return ""
	}
	if !filepath.IsAbs(home) || strings.ContainsRune(home, '\x00') {
		return "fx_home_invalid"
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "fx_home_unresolved"
	}
	if filepath.Clean(home) != filepath.Join(userHome, ".fx") {
		return "fx_home_unsupported"
	}
	return ""
}
