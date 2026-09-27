// Package nativecli holds what every package needs to launch an installed
// harness CLI without handing it the parent's secrets: an allowlisted operating
// environment and Claude's restricted argument set.
package nativecli

import (
	"os"
	"runtime"
	"strings"
)

// Operating contains only OS context needed to find native CLIs and their
// login/cache directories. It never inherits arbitrary provider variables, API
// keys, proxy credentials, integration secrets, or model overrides.
func Operating(goos string, getenv func(string) string) []string {
	keys := []string{"PATH", "HOME", "USER"}
	if goos == "windows" {
		// Windows home/config/cache lookup uses these directories instead of HOME.
		// SystemRoot and COMSPEC support native process startup; PATHEXT supports
		// executable discovery in children. These are paths, not account credentials.
		keys = append(keys, "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "COMSPEC", "PATHEXT")
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// Native is this process's operating environment with its temporary directory.
func Native() []string {
	return WithTemporaryDirectory(Operating(runtime.GOOS, os.Getenv), runtime.GOOS, os.TempDir())
}

// Without removes the named variables. Environment keys are case-insensitive
// on Windows; canonicalizing for removal on every platform also keeps
// synthetic probe construction deterministic.
func Without(env []string, names ...string) []string {
	denied := make(map[string]bool, len(names))
	for _, name := range names {
		denied[strings.ToUpper(name)] = true
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && !denied[strings.ToUpper(key)] {
			out = append(out, entry)
		}
	}
	return out
}

// Override sets each KEY=value entry, replacing any existing value.
func Override(env []string, entries ...string) []string {
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		env = append(Without(env, key), entry)
	}
	return env
}

func WithTemporaryDirectory(env []string, goos, dir string) []string {
	out := Without(env, "TMPDIR", "TMP", "TEMP")
	out = append(out, "TMPDIR="+dir)
	if goos == "windows" {
		out = append(out, "TMP="+dir, "TEMP="+dir)
	}
	return out
}

// Isolated removes native account discovery from local dummy probes and
// catalog reads. The provider home is supplied explicitly afterward.
func Isolated(env []string, goos, dir string) []string {
	out := Without(env, "HOME", "USER", "USERNAME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH", "CODEX_HOME", "CLAUDE_CONFIG_DIR")
	out = append(out, "HOME="+dir)
	if goos == "windows" {
		out = append(out, "USERPROFILE="+dir, "APPDATA="+dir, "LOCALAPPDATA="+dir)
	}
	return WithTemporaryDirectory(out, goos, dir)
}
