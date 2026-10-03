package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Nested reports whether inner is outer or lies inside it. Both are absolute
// and resolved. Paths are compared element by element, without case where the
// file system usually has none; then each of inner's ancestors, inner
// included, is compared with outer by identity, which catches a spelling the
// comparison missed. Failing to read outer counts as nested.
func Nested(outer, inner string) bool {
	if Within(outer, inner) {
		return true
	}
	target, err := os.Stat(outer)
	if err != nil {
		return true
	}
	for dir := inner; ; {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, target) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// Within compares cleaned path elements, folding case on macOS and Windows.
// It does not resolve links or prove filesystem containment.
func Within(outer, inner string) bool {
	split := func(p string) []string {
		return strings.FieldsFunc(filepath.Clean(p), func(r rune) bool { return r == filepath.Separator })
	}
	o, i := split(outer), split(inner)
	if filepath.VolumeName(outer) != "" || filepath.VolumeName(inner) != "" {
		if !strings.EqualFold(filepath.VolumeName(outer), filepath.VolumeName(inner)) {
			return false
		}
	}
	if len(o) > len(i) {
		return false
	}
	fold := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	for n := range o {
		if o[n] != i[n] && !(fold && strings.EqualFold(o[n], i[n])) {
			return false
		}
	}
	return true
}
