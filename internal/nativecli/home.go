package nativecli

import (
	"os"
	"path/filepath"
	"strings"
)

// Every CLI engine launches with its native login from one selected home,
// and with nothing else from the parent: no API key, integration secret or
// model override. These say which home, and build that environment. Each
// returns a fixed refusal code, never a typed error, so completion, catalog
// and the other launchers each report it in their own vocabulary.

// CodexHome resolves the Codex login home a launch uses: the selected one,
// else CODEX_HOME, else ~/.codex.
func CodexHome(home string) (string, string) {
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", "codex_home_unresolved"
		}
		home = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(home) {
		return "", "codex_home_invalid"
	}
	return filepath.Clean(home), ""
}

// CodexEnvironment is the operating environment with the resolved Codex home.
// Codex resolves its own login store there.
func CodexEnvironment(home string) ([]string, string) {
	selected, code := CodexHome(home)
	if code != "" {
		return nil, code
	}
	return append(Native(), "CODEX_HOME="+selected), ""
}

// ClaudeHomeProblem accepts native or explicitly configured login storage:
// an empty home, or an absolute path that is a directory or not yet made.
func ClaudeHomeProblem(home string) string {
	if home == "" {
		return ""
	}
	if !filepath.IsAbs(home) || strings.ContainsRune(home, '\x00') {
		return "claude_home_invalid"
	}
	if stat, err := os.Stat(home); err == nil && !stat.IsDir() {
		return "claude_home_not_directory"
	} else if err != nil && !os.IsNotExist(err) {
		return "claude_home_unavailable"
	}
	return ""
}

// ClaudeEnvironment is the operating environment, with Claude's nonessential
// traffic, updates, telemetry and retries off, and CLAUDE_CONFIG_DIR only for
// a home other than the native ~/.claude. USER stays, because Claude needs it
// for its macOS keychain lookup; auth is resolved natively, including a
// keychain refresh.
func ClaudeEnvironment(home string) ([]string, string) {
	if code := ClaudeHomeProblem(home); code != "" {
		return nil, code
	}
	env := append(Native(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "MAX_RETRIES=0")
	nativeHome, _ := os.UserHomeDir()
	if home != "" && filepath.Clean(home) != filepath.Join(nativeHome, ".claude") {
		env = append(env, "CLAUDE_CONFIG_DIR="+home)
	}
	return env, ""
}

// GrokEnvironment is the operating environment with the selected GROK_HOME
// and the reduced-telemetry overrides. An empty home keeps an absolute ambient
// GROK_HOME, as Codex keeps CODEX_HOME, and otherwise Grok's default.
func GrokEnvironment(home string) ([]string, string) {
	env := Native()
	if home == "" {
		home = os.Getenv("GROK_HOME")
		if !filepath.IsAbs(home) {
			home = ""
		}
	}
	if home != "" {
		if !filepath.IsAbs(home) || strings.ContainsRune(home, '\x00') {
			return nil, "grok_home_invalid"
		}
		env = Override(env, "GROK_HOME="+filepath.Clean(home))
	}
	return Override(env, GrokReducedTelemetry...), ""
}
