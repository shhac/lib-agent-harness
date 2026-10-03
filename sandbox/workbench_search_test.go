package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestWorkbenchSearchBoundsAndPaths(t *testing.T) {
	w, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "docs", "one.txt"), "alpha.x\nalpha-x\n"+strings.Repeat("界", 200)+"\n")
	writeFile(t, filepath.Join(work, "docs", "two.go"), "alpha.x\n")
	writeFile(t, filepath.Join(work, ".git", "hidden"), "alpha.x")
	writeFile(t, filepath.Join(work, ".harness-workbench-test.tmp"), "alpha.x")
	symlink(t, "docs/one.txt", filepath.Join(work, "link"))
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"pattern": "alpha.x", "literal": true, "path": "docs", "glob": "*.txt"}, "docs/one.txt:1: alpha.x"},
		{map[string]any{"pattern": "alpha.x", "path": "docs/one.txt"}, "docs/one.txt:1: alpha.x\ndocs/one.txt:2: alpha-x"},
		{map[string]any{"pattern": "alpha", "glob": "docs/*.go"}, "docs/two.go:1: alpha.x"},
		{map[string]any{"pattern": "["}, wbArgumentsInvalid},
		{map[string]any{"pattern": "界", "path": "docs/one.txt"}, "docs/one.txt:3: " + strings.Repeat("界", (400-len("docs/one.txt:3: "))/3)},
	} {
		got := call(t, w, workbenchSearchFiles, tc.args)
		if got.Content != tc.want && !strings.Contains(got.Content, tc.want) {
			t.Fatalf("%v: %q want %q", tc.args, got.Content, tc.want)
		}
		if strings.Contains(got.Content, "hidden") || strings.Contains(got.Content, "link:") || !utf8.ValidString(got.Content) {
			t.Fatal(got.Content)
		}
	}
	writeFile(t, filepath.Join(work, "many"), strings.Repeat("match\n", 201))
	got := call(t, w, workbenchSearchFiles, map[string]any{"pattern": "match", "path": "many"})
	if strings.Count(got.Content, ": match") != 200 || !strings.Contains(got.Content, "truncated") {
		t.Fatal(got.Content)
	}
	w.budget = minWorkbenchResult
	got = call(t, w, workbenchSearchFiles, map[string]any{"pattern": "match", "path": "many"})
	if len(got.Content) > w.budget || !strings.Contains(got.Content, "truncated") {
		t.Fatalf("%d: %q", len(got.Content), got.Content)
	}
}

func TestWorkbenchSearchSkipsUnsafeFiles(t *testing.T) {
	w, work, outside := testWorkspace(t)
	writeFile(t, filepath.Join(work, "binary"), "\x00"+outsideMarker)
	writeFile(t, filepath.Join(work, "invalid"), "\xff"+outsideMarker)
	writeFile(t, filepath.Join(work, "large"), strings.Repeat("a", maxSearchFileBytes+1))
	testenv.SkipIfRefused(t, "creating a hard link", os.Link(filepath.Join(outside, "secret.txt"), filepath.Join(work, "linked")))
	got := call(t, w, workbenchSearchFiles, map[string]any{"pattern": outsideMarker})
	for _, code := range []string{wbLinked + "=1", wbNotText + "=2", wbTooLarge + "=1"} {
		if !strings.Contains(got.Content, code) {
			t.Fatal(got.Content)
		}
	}
	if strings.Contains(got.Content, outsideMarker) {
		t.Fatal(got.Content)
	}
	got = call(t, w, workbenchReadFile, map[string]any{"path": "linked"})
	if !strings.Contains(got.Content, wbLinked) {
		t.Fatal(got.Content)
	}
}

func TestWorkbenchSearchCancellation(t *testing.T) {
	w, _, _ := testWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	w.step = cancel
	_, err := w.searchFiles(ctx, json.RawMessage(`{"pattern":"inside"}`))
	if err != context.Canceled {
		t.Fatal(err)
	}
}

func TestWorkbenchSearchIncompleteAndVisitedBounds(t *testing.T) {
	w, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "docs", "one"), "inside")
	w.dirFault = func(rel string, batch int) error {
		if rel == "docs" {
			return fs.ErrPermission
		}
		return nil
	}
	got := call(t, w, workbenchSearchFiles, map[string]any{"pattern": "inside"})
	if !strings.Contains(got.Content, "a.txt:1: inside") || !strings.Contains(got.Content, "directories_incomplete=1") {
		t.Fatal(got.Content)
	}
	w.dirFault = func(string, int) error { return fs.ErrPermission }
	got = call(t, w, workbenchSearchFiles, map[string]any{"pattern": "inside"})
	if !got.IsError || !strings.Contains(got.Content, wbUnreadable) {
		t.Fatal(got.Content)
	}
	w.dirFault = nil
	w.maxVisited = 1
	got = call(t, w, workbenchSearchFiles, map[string]any{"pattern": "inside"})
	if !strings.Contains(got.Content, "truncated") {
		t.Fatal(got.Content)
	}
}

func TestWorkbenchSearchNumericLineOrder(t *testing.T) {
	w, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "numbers"), strings.Repeat("match\n", 12))
	got := call(t, w, workbenchSearchFiles, map[string]any{"path": "numbers", "pattern": "match"})
	lines := strings.Split(got.Content, "\n")
	for i, line := range lines {
		want := fmt.Sprintf("numbers:%d: match", i+1)
		if line != want {
			t.Fatalf("line %d: %q, want %q", i+1, line, want)
		}
	}
}
