package restrict

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Codex has no flag that ignores inherited configuration when it runs as an app
// server: `--ignore-user-config` and `--ignore-rules` exist only on `exec`, and
// overriding the table with `-c mcp_servers={}` does not clear entries already
// declared in config.toml — both verified against the installed CLI, where a
// stray server declared in config.toml was observed starting.
//
// So inherited configuration is handled the only way that actually works and
// keeps the login where it is: the selected home is inspected, and a home that
// declares tool-bearing configuration is refused. Nothing is copied, nothing is
// linked, and the CLI's own credential storage is untouched.

// InheritedConfig names a configuration key that would give a restricted
// session capabilities its caller did not configure.
type InheritedConfig struct{ Key string }

func (e *InheritedConfig) Error() string {
	return "harness home declares " + e.Key
}

// Tool-bearing top-level keys. Each one can introduce execution or data reach
// that a restricted session's tool surface does not describe.
var inheritedKeys = []string{"mcp_servers", "hooks", "plugins", "projects", "skills", "agents", "apps"}

// InspectCodexHome reports the first tool-bearing table a home's config.toml
// declares. A home with no config.toml, or one declaring only ordinary settings,
// is fine: `-c` overrides take precedence over those.
//
// This is a structural scan, not a TOML parser: it looks for table headers and
// dotted assignments at the start of a line, which is how these are written.
// Anything it cannot read is refused rather than assumed harmless.
func InspectCodexHome(home string) error {
	if home == "" {
		return nil
	}
	path := filepath.Join(home, "config.toml")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &InheritedConfig{Key: "an unreadable config.toml"}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > 1<<20 {
		return &InheritedConfig{Key: "an unreadable config.toml"}
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := ""
		switch {
		case strings.HasPrefix(line, "[["):
			name = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]"))
		case strings.HasPrefix(line, "["):
			name = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
		default:
			key, _, found := strings.Cut(line, "=")
			if !found {
				continue
			}
			name = strings.TrimSpace(key)
		}
		root, _, _ := strings.Cut(strings.Trim(name, `"'`), ".")
		root = strings.Trim(strings.TrimSpace(root), `"'`)
		for _, forbidden := range inheritedKeys {
			if root == forbidden {
				return &InheritedConfig{Key: forbidden}
			}
		}
	}
	if scanner.Err() != nil {
		return &InheritedConfig{Key: "an unreadable config.toml"}
	}
	return nil
}
