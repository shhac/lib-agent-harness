//go:build darwin || linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Admission changes must invalidate both platform proof keys.
const workbenchPathVersion = "readable-path-v2"

// Capture before commands are admitted. Later host writes use this descriptor,
// never a pathname that an earlier command may have replaced with a symlink.
func (l *workbenchLayout) anchorScratch() error {
	info, err := os.Lstat(l.Tmp)
	if err != nil || !info.IsDir() || !privateStateDir(info) {
		return stateError(StateUnusable)
	}
	canonical, err := filepath.EvalSymlinks(l.Tmp)
	if err != nil || canonical != l.Tmp {
		return stateError(StateUnusable)
	}
	r, err := os.OpenRoot(l.Tmp)
	if err != nil {
		return stateError(StateUnusable)
	}
	l.scratch, l.scratchIdentity = r, info
	if !l.scratchMatches() {
		r.Close()
		l.scratch = nil
		return stateError(StateUnusable)
	}
	return nil
}

func (l workbenchLayout) scratchMatches() bool {
	if l.scratch == nil || l.scratchIdentity == nil {
		return false
	}
	info, err := os.Lstat(l.Tmp)
	if err != nil || !info.IsDir() || !privateStateDir(info) || !os.SameFile(info, l.scratchIdentity) {
		return false
	}
	if info.Sys().(*syscall.Stat_t).Uid != l.scratchIdentity.Sys().(*syscall.Stat_t).Uid {
		return false
	}
	canonical, err := filepath.EvalSymlinks(l.Tmp)
	if err != nil || canonical != l.Tmp {
		return false
	}
	anchored, err := l.scratch.Stat(".")
	return err == nil && privateStateDir(anchored) && os.SameFile(info, anchored) && anchored.Sys().(*syscall.Stat_t).Uid == l.scratchIdentity.Sys().(*syscall.Stat_t).Uid
}

func (l workbenchLayout) readPaths() []string {
	paths := append([]string{}, l.System...)
	return append(append(paths, l.Read...), l.Work, l.Home, l.Tmp)
}

// Metadata ancestors never authorize descendants. Preserve platform exclusions.
func (l workbenchLayout) readsDirectory(path string) bool {
	if workbenchReadDirectoryDenied(path) {
		return false
	}
	for _, p := range l.System {
		if workbenchSystemContains(p, path) {
			return true
		}
	}
	paths := append(append([]string{}, l.Read...), l.Work, l.Home, l.Tmp)
	paths = append(paths, workbenchPublicReadPaths()...)
	for _, p := range paths {
		if lexicallyWithin(p, path) {
			return true
		}
	}
	return false
}

type workbenchPathDrop struct{ entry, reason string }

func filterWorkbenchPath(l workbenchLayout, source string) (string, []workbenchPathDrop) {
	var kept []string
	var dropped []workbenchPathDrop
	for _, entry := range strings.Split(source, string(os.PathListSeparator)) {
		reason, resolved := "", ""
		switch {
		case entry == "":
			reason = "empty"
		case !filepath.IsAbs(entry):
			reason = "relative"
		default:
			var err error
			resolved, err = filepath.EvalSymlinks(entry)
			if err != nil {
				reason = "unresolvable"
			} else if info, err := os.Stat(resolved); err != nil {
				reason = "unresolvable"
			} else if !info.IsDir() {
				reason = "not-directory"
			} else if strings.ContainsRune(resolved, os.PathListSeparator) {
				reason = "path-separator"
			} else if !l.readsDirectory(resolved) {
				reason = "outside-read-set"
			}
		}
		if reason != "" {
			dropped = append(dropped, workbenchPathDrop{entry, reason})
		} else {
			kept = append(kept, resolved)
		}
	}
	return strings.Join(kept, string(os.PathListSeparator)), dropped
}

// Recheck captured merged PATH per invocation without mutating the source.
func workbenchCommandEnvironment(l workbenchLayout, source []string) ([]string, []workbenchPathDrop, error) {
	if l.scratch != nil && !l.scratchMatches() {
		return nil, nil, stateError(StateUnusable)
	}
	env := make([]string, 0, len(source)+1)
	path := ""
	for _, value := range source {
		if strings.HasPrefix(value, "PATH=") {
			path = strings.TrimPrefix(value, "PATH=")
		} else {
			env = append(env, value)
		}
	}
	path, dropped := filterWorkbenchPath(l, path)
	if path == "" {
		// A scratch path containing ':' cannot be represented as one Unix PATH
		// entry. Refuse rather than introduce implicit relative/default lookup.
		if !filepath.IsAbs(l.Tmp) || strings.ContainsRune(l.Tmp, os.PathListSeparator) {
			return nil, nil, stateError(StateUnusable)
		}
		if !l.scratchMatches() {
			return nil, nil, stateError(StateUnusable)
		}
		name := "empty-path-" + newID()
		if err := l.scratch.Mkdir(name, 0700); err != nil {
			return nil, nil, stateError(StateUnusable)
		}
		if !l.scratchMatches() {
			_ = l.scratch.Remove(name)
			return nil, nil, stateError(StateUnusable)
		}
		path = filepath.Join(l.Tmp, name)
	}
	return append(env, "PATH="+path), dropped, nil
}

// The escaped note consumes the existing stderr budget.
func workbenchPathReport(dropped []workbenchPathDrop, limit int) string {
	if len(dropped) == 0 || limit < 80 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[harness PATH: dropped ")
	shown := 0
	for _, d := range dropped {
		item := strconv.Quote(d.entry) + " (" + d.reason + "); "
		if b.Len()+len(item)+64 > limit {
			break
		}
		b.WriteString(item)
		shown++
	}
	if shown < len(dropped) {
		fmt.Fprintf(&b, "%d entries omitted", len(dropped)-shown)
	}
	b.WriteString("]\n")
	return b.String()
}
