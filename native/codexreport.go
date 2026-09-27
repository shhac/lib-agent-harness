package native

import (
	"os"
	"path/filepath"
	"strings"
)

// codexReport is where one Codex invocation takes its schema and leaves its
// final message. Codex reads a schema only from a file and writes its report
// only to one, so the library owns both, in a directory made for one
// invocation: a resumed run can never read an earlier run's report.
//
// The directory must not be one the agent can write to, or a workspace-write
// agent could forge its own report. Codex's workspace-write sandbox allows the
// working directory and, by default, the system temporary directories, so the
// user cache directory is tried first and a temporary one only when that is
// unavailable. Neither is accepted inside the working directory.
type codexReport struct {
	dir    string
	schema string
	output string
}

// reportRoots lists the parents a report directory may be made in, most
// private first. Tests replace it to keep their files in a temporary tree.
var reportRoots = func() []string {
	var roots []string
	if cache, err := os.UserCacheDir(); err == nil {
		roots = append(roots, filepath.Join(cache, "lib-agent-harness", "native-reports"))
	}
	return append(roots, os.TempDir())
}

func newCodexReport(workDir, schema string) (*codexReport, error) {
	dir, err := privateDir(workDir, "codex-")
	if err != nil {
		return nil, err
	}
	report := &codexReport{dir: dir, schema: filepath.Join(dir, "schema.json"), output: filepath.Join(dir, "report.json")}
	if err := os.WriteFile(report.schema, []byte(schema), 0o600); err != nil {
		report.remove()
		return nil, err
	}
	return report, nil
}

// privateDir makes a directory for one invocation under the first usable
// report root outside the working directory.
func privateDir(workDir, prefix string) (string, error) {
	workspace, err := resolvedWorkDir(workDir)
	if err != nil {
		return "", err
	}
	var lastErr error = os.ErrNotExist
	for _, root := range reportRoots() {
		if within(workspace, resolved(root)) {
			continue
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			lastErr = err
			continue
		}
		dir, err := os.MkdirTemp(root, prefix)
		if err != nil {
			lastErr = err
			continue
		}
		return dir, nil
	}
	return "", lastErr
}

func (r *codexReport) remove() {
	if r != nil {
		_ = os.RemoveAll(r.dir)
	}
}

// resolvedWorkDir is where Codex will run: WorkDir, or this process's own
// directory when it is empty.
func resolvedWorkDir(workDir string) (string, error) {
	if workDir == "" {
		var err error
		if workDir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", err
	}
	return resolved(abs), nil
}

// resolved follows symlinks as far as the path exists, so an alias such as
// macOS's /tmp -> /private/tmp cannot hide containment.
func resolved(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent, base := filepath.Split(path)
	if parent == "" || filepath.Clean(parent) == path {
		return path
	}
	return filepath.Join(resolved(parent), base)
}

func within(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
