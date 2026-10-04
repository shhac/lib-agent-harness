package sandbox

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// shortName is the 8.3 alias Windows gives the last element of name, or ""
// when the volume makes none.
func shortName(t *testing.T, name string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	short := filepath.Base(windows.UTF16ToString(buf[:n]))
	if strings.EqualFold(short, filepath.Base(name)) {
		return ""
	}
	return short
}

// An 8.3 short name reaches a file under a name no spelling rule knows. The
// tools judge what they open by the name the file system gives it, so .git
// and the reserved temporaries stay hidden under their aliases too.
func TestWorkbenchShortNamesDoNotUnhide(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, ".git", "objects", "pack", "x"), "")
	reserved := ".harness-workbench-0123456789abcdef0123456789abcdef-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(work, reserved), "half written\n")
	writeFile(t, filepath.Join(work, ".harness-workbench-dir.tmp", "x"), "hidden\n")
	git := shortName(t, filepath.Join(work, ".git"))
	file := shortName(t, filepath.Join(work, reserved))
	dir := shortName(t, filepath.Join(work, ".harness-workbench-dir.tmp"))
	if git == "" || file == "" || dir == "" {
		testenv.SkipIfRefused(t, "the temporary volume makes no 8.3 short names", fs.ErrPermission)
	}
	for _, path := range []string{git, git + "/objects", git + "/objects/pack", dir} {
		refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": path}), wbNotListed)
	}
	refusedWith(t, read(t, ws, file), wbReserved)
	refusedWith(t, read(t, ws, dir+"/x"), wbReserved)
	// .git stays readable, under any name.
	if r := read(t, ws, git+"/objects/pack/x"); r.IsError {
		t.Fatalf("%+v", r)
	}
}
