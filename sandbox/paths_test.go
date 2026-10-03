package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSharedPathRules(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Outer")
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		outer, inner string
		within       bool
	}{
		{root, root, true}, {root, child, true}, {child, root, false},
		{root, root + "-sibling", false},
		{root, filepath.Join(root, "child", ".."), true},
		{root, strings.ToLower(root), runtime.GOOS == "darwin" || runtime.GOOS == "windows"},
	} {
		if got := Within(tc.outer, tc.inner); got != tc.within {
			t.Fatalf("Within(%q,%q)=%t", tc.outer, tc.inner, got)
		}
	}
	if !Nested(root, child) || Nested(child, root) {
		t.Fatal("ancestor comparison changed")
	}
	if !Nested(filepath.Join(base, "missing"), child) {
		t.Fatal("unreadable outer must fail closed")
	}
	link := filepath.Join(base, "alias")
	symlink(t, root, link)
	if !Nested(root, filepath.Join(link, "child")) {
		t.Fatal("identity ancestry missed alias")
	}
}
