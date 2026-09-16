package completion

import (
	"os"
	"runtime"
	"strings"
)

// operatingEnvironment contains only OS context needed to find native CLIs and
// their login/cache directories. It never inherits arbitrary provider variables,
// API keys, proxy credentials, integration secrets, or model overrides.
func operatingEnvironment(goos string, getenv func(string) string) []string {
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

func nativeOperatingEnvironment() []string {
	return withTemporaryDirectory(operatingEnvironment(runtime.GOOS, os.Getenv), runtime.GOOS, os.TempDir())
}

// Environment keys are case-insensitive on Windows; canonicalizing for removal
// on every platform also keeps synthetic probe construction deterministic.
func withoutEnvironment(env []string, names ...string) []string {
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

func withTemporaryDirectory(env []string, goos, dir string) []string {
	out := withoutEnvironment(env, "TMPDIR", "TMP", "TEMP")
	out = append(out, "TMPDIR="+dir)
	if goos == "windows" {
		out = append(out, "TMP="+dir, "TEMP="+dir)
	}
	return out
}

// isolatedOperatingEnvironment removes native account discovery from local
// dummy probes/catalog reads. The provider home is supplied explicitly afterward.
func isolatedOperatingEnvironment(env []string, goos, dir string) []string {
	out := withoutEnvironment(env, "HOME", "USER", "USERNAME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH", "CODEX_HOME", "CLAUDE_CONFIG_DIR")
	out = append(out, "HOME="+dir)
	if goos == "windows" {
		out = append(out, "USERPROFILE="+dir, "APPDATA="+dir, "LOCALAPPDATA="+dir)
	}
	return withTemporaryDirectory(out, goos, dir)
}
