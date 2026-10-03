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
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if rel, err := filepath.Rel(dir, home); home != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Sprintf("sandbox read path %q would reopen the home directory", dir)
		}
		out = append(out, dir)
	}
	return out, ""
}
