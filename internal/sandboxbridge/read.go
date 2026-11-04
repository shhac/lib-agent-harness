package sandboxbridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadDirs resolves each extra readable directory to the path the
// sandbox will match, and refuses any that would reopen the home directory.
func ReadDirs(dirs []string) ([]string, string) {
	home, _ := os.UserHomeDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return nil, fmt.Sprintf("sandbox read path %q must be a clean absolute path", dir)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, "sandbox read path must name an existing directory"
		}
		dir = resolved
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, "sandbox read path must name an existing directory"
		}
		if strings.ContainsAny(dir, "*?[]()\n\r") {
			return nil, "sandbox read path contains permission pattern characters"
		}
		parts := strings.Split(filepath.ToSlash(dir), "/")
		for i, part := range parts {
			switch part {
			case ".claude", ".codex":
				// Explicit instruction skills are data; admitting their narrow subtree
				// does not admit the surrounding native login/configuration directory.
				if i+1 < len(parts) && parts[i+1] == "skills" {
					continue
				}
				return nil, "sandbox read path names a private configuration directory"
			case ".git", ".ssh", ".aws", ".config":
				return nil, "sandbox read path names a private configuration directory"
			}
		}
		if rel, err := filepath.Rel(dir, home); home != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Sprintf("sandbox read path %q would reopen the home directory", dir)
		}
		switch filepath.ToSlash(dir) {
		case "/tmp", "/private/tmp", "/var", "/private/var", "/private", "/usr", "/opt", "/etc", "/private/etc", "/Applications", "/System", "/Library", "/Volumes", "/mnt", "/media", "/srv", "/run", "/home", "/root", "/Users":
			return nil, "sandbox read path names a broad system root"
		}
		out = append(out, dir)
	}
	return out, ""
}
