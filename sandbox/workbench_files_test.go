package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

const outsideMarker = "OUTSIDE-MARKER-4d1c"

// testWorkspace opens a workspace on base/work beside base/outside, which
// holds a secret marked with outsideMarker.
func testWorkspace(t *testing.T) (ws *Workspace, work, outside string) {
	t.Helper()
	base := t.TempDir()
	work, outside = filepath.Join(base, "work"), filepath.Join(base, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), outsideMarker+"\n")
	writeFile(t, filepath.Join(work, "a.txt"), "inside\n")
	ws, err := OpenWorkspace(Config{Root: work})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ws.close)
	return ws, work, outside
}

func call(t *testing.T, ws *Workspace, tool string, args any) Result {
	t.Helper()
	raw, _ := json.Marshal(args)
	var (
		result Result
		err    error
	)
	switch tool {
	case workbenchWriteFile, workbenchEditFile:
		testenv.RequireAtomicWrite(t) // An outer sandbox protects file-tool temporaries.
		result, err = ws.writeFile(context.Background(), tool, raw)
	case workbenchReadFile:
		result, err = ws.readFile(context.Background(), raw)
	case workbenchListFiles:
		result, err = ws.listFiles(context.Background(), raw)
	case workbenchSearchFiles:
		result, err = ws.searchFiles(context.Background(), raw)
	}
	if err != nil && !errors.Is(err, errWorkbenchWriteUnknown) && !(ToolAvailability(tool) != "" && errors.Is(err, ErrUnsupported)) {
		t.Fatalf("%s %s: %v", tool, raw, err)
	}
	if ws.handles.Load() != 0 {
		t.Fatalf("%s %s left %d handles open", tool, raw, ws.handles.Load())
	}
	return result
}

func read(t *testing.T, ws *Workspace, path string) Result {
	t.Helper()
	return call(t, ws, workbenchReadFile, map[string]any{"path": path})
}

func refusedWith(t *testing.T, r Result, code string) {
	t.Helper()
	if !r.IsError || !strings.Contains(r.Content, " error: "+code) {
		t.Fatalf("want %s, got %+v", code, r)
	}
	if strings.Contains(r.Content, outsideMarker) {
		t.Fatal("the outside marker leaked")
	}
}

func TestWorkbenchPathsStayInside(t *testing.T) {
	ws, work, outside := testWorkspace(t)
	if r := read(t, ws, "a.txt"); ToolAvailability(workbenchReadFile) != "" {
		refusedWith(t, r, RefusedNotOffered)
	} else if r.IsError || r.Content != "inside\n" {
		t.Fatalf("%+v", r)
	}
	absolute := filepath.ToSlash(filepath.Join(outside, "secret.txt"))
	for path, code := range map[string]string{
		"../outside/secret.txt":   wbOutside,
		"a/../../outside/x":       wbOutside,
		"..":                      wbOutside,
		absolute:                  wbPathInvalid,
		"/etc/passwd":             wbPathInvalid,
		"C:/Windows/win.ini":      wbPathInvalid,
		"C:secret.txt":            wbPathInvalid,
		`..\outside\secret.txt`:   wbPathInvalid,
		`sub\a.txt`:               wbPathInvalid,
		"a.txt\x00":               wbPathInvalid,
		"CON":                     wbPathInvalid,
		"nul.txt":                 wbPathInvalid,
		"AUX ":                    wbPathInvalid,
		"docs/com1.md":            wbPathInvalid,
		"":                        wbPathInvalid,
		"missing.txt":             wbNotFound,
		strings.Repeat("a/", 600): wbPathInvalid,
	} {
		refusedWith(t, read(t, ws, path), effectiveContentCode(workbenchReadFile, code))
	}
	for path, code := range map[string]string{"../outside": wbOutside, "/": wbPathInvalid, "NUL": wbPathInvalid, "a.txt": wbNotDirectory} {
		refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": path}), code)
	}
	if r := call(t, ws, workbenchReadFile, json.RawMessage(`{"path":5}`)); !r.IsError || !strings.Contains(r.Content, effectiveContentCode(workbenchReadFile, wbArgumentsInvalid)) {
		t.Fatalf("%+v", r)
	}
	if r := read(t, ws, "."); !r.IsError {
		t.Fatalf("the root read as a file: %+v", r)
	}
	if strings.Contains(read(t, ws, "missing.txt").Content, work) {
		t.Fatal("a host path reached a result")
	}
}

func TestWorkbenchLinksStayInside(t *testing.T) {
	ws, work, outside := testWorkspace(t)
	symlink(t, filepath.Join("..", "outside", "secret.txt"), filepath.Join(work, "relative"))
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(work, "absolute"))
	symlink(t, "hop2", filepath.Join(work, "hop1"))
	symlink(t, filepath.Join("..", "outside"), filepath.Join(work, "hop2"))
	symlink(t, outside, filepath.Join(work, "dir"))
	symlink(t, "a.txt", filepath.Join(work, "alias"))
	for _, path := range []string{"relative", "absolute", "hop1/secret.txt", "hop2/secret.txt", "dir/secret.txt"} {
		refusedWith(t, read(t, ws, path), effectiveContentCode(workbenchReadFile, wbOutside))
	}
	// Listing never follows a link, so one out is refused as a link before
	// the root is ever asked to follow it.
	refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": "dir"}), wbIsSymlink)
	refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": "hop2"}), wbIsSymlink)
	// A link that stays inside is followed for a read.
	if r := read(t, ws, "alias"); ToolAvailability(workbenchReadFile) != "" {
		refusedWith(t, r, RefusedNotOffered)
	} else if r.IsError || r.Content != "inside\n" {
		t.Fatalf("%+v", r)
	}
	// Listing names links and never follows them.
	listing := call(t, ws, workbenchListFiles, map[string]any{"depth": 8})
	for _, line := range []string{"a.txt", "absolute [link]", "alias [link]", "dir [link]", "hop1 [link]", "relative [link]"} {
		if !strings.Contains("\n"+listing.Content+"\n", "\n"+line+"\n") {
			t.Errorf("listing lacks %q:\n%s", line, listing.Content)
		}
	}
	if strings.Contains(listing.Content, "secret.txt") {
		t.Fatalf("the listing followed a link:\n%s", listing.Content)
	}
}

// list_files follows no link, not even one inside the workspace that it was
// asked for by name, in the last component or on the way to it.
func TestWorkbenchListFollowsNoRequestedLink(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "real", "sub", "inner.txt"), "")
	symlink(t, "real", filepath.Join(work, "link-to-dir"))
	refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": "link-to-dir"}), wbIsSymlink)
	refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": "link-to-dir/sub"}), wbThroughSymlink)
	if r := call(t, ws, workbenchListFiles, map[string]any{"path": "real"}); r.Content != "real/sub/\nreal/sub/inner.txt" {
		t.Fatalf("%q", r.Content)
	}
	// read_file still follows a link that stays inside, as the design says.
	if r := read(t, ws, "link-to-dir/sub/inner.txt"); r.IsError && ToolAvailability(workbenchReadFile) == "" {
		t.Fatalf("%+v", r)
	}
}

// .git and the reserved temporaries are hidden from list_files however they
// are named: asked for directly, reached through a path, or spelled another
// way. read_file refuses anything under a reserved temporary name, and still
// reads .git.
func TestWorkbenchHiddenNamesStayHiddenWhenAskedFor(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(work, ".git", "refs", "heads", "main"), "0000\n")
	reserved := ".harness-workbench-0123456789abcdef0123456789abcdef-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(work, reserved, "inside.txt"), "hidden\n")
	writeFile(t, filepath.Join(work, "src", ".git", "config"), "[core]\n")
	for _, path := range []string{".git", ".git/refs", ".GIT", ".git.", reserved, reserved + "/x", "src/.git", ".HARNESS-WORKBENCH-x.TMP"} {
		refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": path}), wbNotListed)
	}
	refusedWith(t, read(t, ws, reserved+"/inside.txt"), effectiveContentCode(workbenchReadFile, wbReserved))
	if r := read(t, ws, ".git/HEAD"); ToolAvailability(workbenchReadFile) != "" {
		refusedWith(t, r, RefusedNotOffered)
	} else if r.IsError {
		t.Fatalf(".git/HEAD is readable: %+v", r)
	}
	// Another spelling of .git that reaches it by the file system, not by
	// name, is refused by identity even when the spelling check is blind to
	// it: this is what stops a Windows short name, which no spelling rule
	// can know.
	if _, err := os.Stat(filepath.Join(work, ".GIT")); err == nil {
		root, err := ws.rootNode()
		if err != nil {
			t.Fatal(err)
		}
		info, err := ws.root.Lstat(".GIT")
		if err != nil {
			t.Fatal(err)
		}
		blind := ws.newCursor(root, walkPolicy{hides: func(string) bool { return false }, git: true})
		defer blind.close()
		if _, err := blind.to(root.child(".GIT", info)); !errors.Is(err, errHidden) {
			t.Fatalf("the workspace's .git was entered under another spelling: %v", err)
		}
	}
	listing := call(t, ws, workbenchListFiles, map[string]any{"depth": 8})
	if strings.Contains(listing.Content, ".git") || strings.Contains(listing.Content, "harness-workbench") {
		t.Fatalf("%q", listing.Content)
	}
}

// The quoted path in an error is bounded as maxQuotedPath says: at most
// four bytes out per byte in, whatever the path holds.
func TestWorkbenchQuotedPathBound(t *testing.T) {
	for name, path := range map[string]string{
		"controls": strings.Repeat("\x01", 1024),
		"invalid":  strings.Repeat("\xff", 1024),
		"C1":       strings.Repeat("\u0085", 512),
		"wide":     strings.Repeat("\U0001F600", 256),
		"mixed":    strings.Repeat("\x01\u0085 ", 200),
	} {
		r := workbenchError(workbenchListFiles, wbPathInvalid, path)
		quoted := strings.TrimPrefix(r.Content, workbenchListFiles+" error: "+wbPathInvalid+": ")
		if len(quoted) > 4*maxQuotedPath+2 {
			t.Errorf("%s: quoted to %d bytes", name, len(quoted))
		}
	}
}

// An ancestor checked and then swapped for a link inside the workspace is not
// followed: the walk opens each component through its parent's handle and
// checks it is the directory Lstat saw, so the swap is caught at the
// ancestor, even though the directory at the end of the link has the same
// shape as the one asked for.
func TestWorkbenchNestedSwapIsNotFollowed(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "a", "b", "real.txt"), "")
	writeFile(t, filepath.Join(work, "c", "b", "via-link.txt"), "")
	root, err := ws.rootNode()
	if err != nil {
		t.Fatal(err)
	}
	aInfo, err := ws.root.Lstat("a")
	if err != nil {
		t.Fatal(err)
	}
	bInfo, err := ws.root.Lstat(filepath.Join("a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	b := root.child("a", aInfo).child("b", bInfo)
	// Swap a for a link to c after both were checked.
	if err := os.Rename(filepath.Join(work, "a"), filepath.Join(work, "a-real")); err != nil {
		t.Fatal(err)
	}
	symlink(t, "c", filepath.Join(work, "a"))
	c := ws.newCursor(root, listPolicy)
	defer c.close()
	if _, err := c.to(b); !errors.Is(err, errChanged) {
		t.Fatalf("the walk followed a swapped ancestor: %v", err)
	}
	if ws.handles.Load() != 0 {
		t.Fatalf("%d handles left open by a refused walk", ws.handles.Load())
	}
	// And asked for by name, the link is refused on the way.
	refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": "a/b"}), wbThroughSymlink)
}

// The same, raced: while an ancestor keeps being swapped between a real
// directory and a link to a look-alike inside the workspace, no listing ever
// shows what is behind the link.
func TestWorkbenchNestedSwapRace(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "real", "b", "real.txt"), "")
	writeFile(t, filepath.Join(work, "c", "b", "via-link.txt"), "")
	symlink(t, "c", filepath.Join(work, "fake"))
	a, real, fake := filepath.Join(work, "a"), filepath.Join(work, "real"), filepath.Join(work, "fake")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, from := range []string{real, fake} {
				if os.Rename(from, a) == nil {
					_ = os.Rename(a, from)
				}
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, args := range []map[string]any{{"path": "a/b"}, {"path": "a"}, {"depth": 3}} {
			r := call(t, ws, workbenchListFiles, args)
			if strings.Contains(r.Content, "a/b/via-link.txt") || (args["path"] != nil && strings.Contains(r.Content, "via-link.txt")) {
				t.Fatalf("a listing of %v followed the swapped link: %q", args, r.Content)
			}
		}
	}
}

// read_file follows a link that stays inside, but never to a reserved
// temporary, and never round a loop.
func TestWorkbenchReadResolvesLinksItself(t *testing.T) {
	if contentDisabled(t, workbenchReadFile) {
		return
	}
	ws, work, _ := testWorkspace(t)
	reserved := ".harness-workbench-0123456789abcdef0123456789abcdef-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(work, "src", reserved), "half written\n")
	writeFile(t, filepath.Join(work, ".harness-workbench-dir.tmp", "x"), "hidden\n")
	symlink(t, filepath.Join("src", reserved), filepath.Join(work, "notes"))
	symlink(t, ".harness-workbench-dir.tmp", filepath.Join(work, "into"))
	symlink(t, "loop2", filepath.Join(work, "loop1"))
	symlink(t, "loop1", filepath.Join(work, "loop2"))
	symlink(t, filepath.Join("src", "..", "a.txt"), filepath.Join(work, "dotted"))
	refusedWith(t, read(t, ws, "notes"), wbReserved)
	refusedWith(t, read(t, ws, "into/x"), wbReserved)
	refusedWith(t, read(t, ws, "loop1"), wbUnreadable)
	if r := read(t, ws, "dotted"); r.IsError || r.Content != "inside\n" {
		t.Fatalf("%+v", r)
	}
}

// A directory swapped for a link to outside while the tools run never leaks
// the outside file.
func TestWorkbenchSwappedDirectoryStaysInside(t *testing.T) {
	ws, work, outside := testWorkspace(t)
	writeFile(t, filepath.Join(work, "real", "secret.txt"), "inside\n")
	symlink(t, outside, filepath.Join(work, "fake"))
	sub, real, fake := filepath.Join(work, "sub"), filepath.Join(work, "real"), filepath.Join(work, "fake")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, from := range []string{real, fake} {
				if os.Rename(from, sub) == nil {
					_ = os.Rename(sub, from)
				}
			}
		}
	}()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, r := range []Result{read(t, ws, "sub/secret.txt"), call(t, ws, workbenchListFiles, map[string]any{"path": "sub"}), call(t, ws, workbenchListFiles, map[string]any{"depth": 3}), call(t, ws, workbenchSearchFiles, map[string]any{"path": "sub", "pattern": outsideMarker}), call(t, ws, workbenchSearchFiles, map[string]any{"path": "sub/secret.txt", "pattern": outsideMarker})} {
			if strings.Contains(r.Content, outsideMarker) {
				close(stop)
				wg.Wait()
				t.Fatalf("the outside marker leaked: %q", r.Content)
			}
		}
	}
	close(stop)
	wg.Wait()
}

func TestWorkbenchReadBounds(t *testing.T) {
	if contentDisabled(t, workbenchReadFile) {
		return
	}
	ws, work, _ := testWorkspace(t)
	var lines strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	writeFile(t, filepath.Join(work, "long.txt"), lines.String())
	r := read(t, ws, "long.txt")
	if r.IsError || strings.Count(r.Content, "\n") != 2000 || !strings.HasPrefix(r.Content, "line 1\n") || !strings.HasSuffix(r.Content, "line 2000\n[read_file: lines 1-2000 of 2500 shown. Continue with offset 2001]") {
		t.Fatalf("first page: %d lines, ends %q", strings.Count(r.Content, "\n"), r.Content[len(r.Content)-80:])
	}
	if r = call(t, ws, workbenchReadFile, map[string]any{"path": "long.txt", "offset": 2001}); r.IsError || !strings.HasPrefix(r.Content, "line 2001\n") || !strings.HasSuffix(r.Content, "line 2500\n") {
		t.Fatalf("second page ends %q", r.Content[len(r.Content)-40:])
	}
	if r = call(t, ws, workbenchReadFile, map[string]any{"path": "long.txt", "offset": 10, "limit": 2}); r.Content != "line 10\nline 11\n[read_file: lines 10-11 of 2500 shown. Continue with offset 12]" {
		t.Fatalf("a window: %q", r.Content)
	}
	if r = call(t, ws, workbenchReadFile, map[string]any{"path": "long.txt", "offset": 3000}); r.IsError || !strings.Contains(r.Content, "past its end") {
		t.Fatalf("past the end: %+v", r)
	}
	if r = call(t, ws, workbenchReadFile, map[string]any{"path": "long.txt", "offset": 0}); !r.IsError {
		t.Fatalf("offset 0: %+v", r)
	}

	wide := strings.Repeat(strings.Repeat("x", 99)+"\n", 1000)
	writeFile(t, filepath.Join(work, "wide.txt"), wide)
	r = read(t, ws, "wide.txt")
	if len(r.Content) > maxWorkbenchResult || !strings.Contains(r.Content, "shown. Continue with offset ") {
		t.Fatalf("a wide file returned %d bytes, ending %q", len(r.Content), r.Content[len(r.Content)-80:])
	}

	writeFile(t, filepath.Join(work, "oneline.txt"), strings.Repeat("y", 100<<10))
	if r = read(t, ws, "oneline.txt"); len(r.Content) > maxWorkbenchResult || !strings.Contains(r.Content, "line 1 is longer than one result") || !strings.Contains(r.Content, "Continue with offset 2") {
		t.Fatalf("one long line: %d bytes", len(r.Content))
	}

	writeFile(t, filepath.Join(work, "empty.txt"), "")
	if r = read(t, ws, "empty.txt"); r.IsError || !strings.Contains(r.Content, "empty") {
		t.Fatalf("%+v", r)
	}
	writeFile(t, filepath.Join(work, "binary.bin"), "\xff\xfe\x00")
	refusedWith(t, read(t, ws, "binary.bin"), wbNotText)
	huge, err := os.Create(filepath.Join(work, "huge.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err = huge.Truncate(maxReadFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = huge.Close()
	refusedWith(t, read(t, ws, "huge.txt"), wbTooLarge)
	refusedWith(t, read(t, ws, "."+string(os.PathSeparator)), wbPathInvalid)
	if err = os.Mkdir(filepath.Join(work, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, read(t, ws, "folder"), wbNotRegular)
}

// A file of millions of one-byte lines, as large as read_file accepts, costs
// about what is shown, not a header per line: line ends are scanned in place.
func TestWorkbenchReadOfManyShortLinesIsBounded(t *testing.T) {
	data := bytes.Repeat([]byte("\n"), maxReadFileBytes)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := readLines(data, 1, maxReadLines, maxWorkbenchResult)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("reading %d lines allocated %d bytes", maxReadFileBytes, allocated)
	}
	if want := strings.Repeat("\n", maxReadLines) + "[read_file: lines 1-2000 of " + strconv.Itoa(maxReadFileBytes) + " shown. Continue with offset 2001]"; got != want {
		t.Fatalf("first page ends %q", got[len(got)-80:])
	}

	if contentDisabled(t, workbenchReadFile) {
		return
	}
	ws, work, _ := testWorkspace(t)
	short := bytes.Repeat([]byte("x\n"), maxReadFileBytes/2)
	if err := os.WriteFile(filepath.Join(work, "short.txt"), short, 0o600); err != nil {
		t.Fatal(err)
	}
	last := maxReadFileBytes / 2
	r := call(t, ws, workbenchReadFile, map[string]any{"path": "short.txt", "offset": last - 1})
	if r.IsError || r.Content != "x\nx\n" {
		t.Fatalf("the last two lines: %+v", r)
	}
	r = call(t, ws, workbenchReadFile, map[string]any{"path": "short.txt", "offset": last + 1})
	if !strings.Contains(r.Content, "the file has "+strconv.Itoa(last)+" lines") {
		t.Fatalf("past the end: %q", r.Content)
	}
}

// A line longer than a result is cut on a character boundary.
func TestWorkbenchLongLineIsCutOnACharacter(t *testing.T) {
	for shift := 0; shift < 4; shift++ {
		data := []byte(strings.Repeat("a", shift) + strings.Repeat("€", 40<<10))
		got := readLines(data, 1, maxReadLines, maxWorkbenchResult)
		if !utf8.ValidString(got) || len(got) > maxWorkbenchResult || !strings.Contains(got, "line 1 is longer than one result") {
			t.Fatalf("shift %d: %d bytes, valid %v", shift, len(got), utf8.ValidString(got))
		}
	}
}

// A result is bounded by the host's MaxResultBytes too, and says where it
// was cut rather than leaving the host to cut it.
func TestWorkbenchResultFitsTheHostLimit(t *testing.T) {
	o := Config{Root: t.TempDir(), Budget: 4096}
	writeFile(t, filepath.Join(o.Root, "wide.txt"), strings.Repeat(strings.Repeat("z", 99)+"\n", 200))
	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(o.Root, "many", fmt.Sprintf("file-%03d.txt", i)), "")
	}
	ws, err := OpenWorkspace(o)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.close()
	r := read(t, ws, "wide.txt")
	if ToolAvailability(workbenchReadFile) != "" {
		refusedWith(t, r, RefusedNotOffered)
	} else if len(r.Content) > 4096 || !strings.Contains(r.Content, "Continue with offset") {
		t.Fatalf("%d bytes: %q", len(r.Content), r.Content[len(r.Content)-80:])
	}
	if len(r.Content) > 4096 {
		t.Fatal("the host would cut the result")
	}
	list := call(t, ws, workbenchListFiles, map[string]any{"path": "many"})
	if len(list.Content) > 4096 || !strings.Contains(list.Content, " omitted. Narrow it with path or depth]") {
		t.Fatalf("%d bytes: %q", len(list.Content), list.Content[len(list.Content)-80:])
	}
	if o.Budget = 0; true {
		if wide, _ := OpenWorkspace(o); wide.budget != maxWorkbenchResult {
			t.Fatalf("default budget %d", wide.budget)
		} else {
			wide.close()
		}
	}
}

// At the smallest result limit a workbench accepts, every answer fits: errors
// quoting the longest or most escaped path, and each kind of closing note,
// together.
func TestWorkbenchAnswersFitTheSmallestLimit(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	ws.budget = minWorkbenchResult
	writeFile(t, filepath.Join(work, "wide.txt"), strings.Repeat(strings.Repeat("w", 99)+"\n", 200))
	writeFile(t, filepath.Join(work, "oneline.txt"), strings.Repeat("é", 8<<10))
	for i := 0; i < 400; i++ {
		writeFile(t, filepath.Join(work, "many", fmt.Sprintf("entry-%03d.txt", i)), "")
		writeFile(t, filepath.Join(work, "many", fmt.Sprintf("sub-%03d", i), "x"), "")
	}
	ws.dirFault = func(native string, batch int) error {
		if strings.HasSuffix(native, "sub-007") {
			return errors.New("injected")
		}
		return nil
	}
	controls := strings.Repeat("\x01", maxWorkbenchPath)
	deep := strings.Repeat("d/", maxWorkbenchPath/2-1) + "x"
	for _, tc := range []struct {
		tool string
		args map[string]any
		note string
	}{
		{workbenchReadFile, map[string]any{"path": controls}, wbPathInvalid},
		{workbenchReadFile, map[string]any{"path": deep}, wbNotFound},
		{workbenchListFiles, map[string]any{"path": controls}, wbPathInvalid},
		{workbenchListFiles, map[string]any{"path": deep}, wbNotFound},
		{workbenchReadFile, map[string]any{"path": "wide.txt"}, "Continue with offset"},
		{workbenchReadFile, map[string]any{"path": "oneline.txt"}, "is longer than one result"},
		{workbenchReadFile, map[string]any{"path": "wide.txt", "offset": 500}, "past its end"},
		{workbenchListFiles, map[string]any{"path": "many"}, "could not be read in full"},
	} {
		r := call(t, ws, tc.tool, tc.args)
		if len(r.Content) > minWorkbenchResult || !strings.Contains(r.Content, effectiveContentCode(tc.tool, tc.note)) {
			t.Errorf("%s %v: %d bytes, ending %q", tc.tool, tc.args["path"], len(r.Content), r.Content[max(0, len(r.Content)-120):])
		}
	}
	list := call(t, ws, workbenchListFiles, map[string]any{"path": "many"})
	if !strings.Contains(list.Content, " omitted. Narrow it with path or depth]\n[list_files: 1 directories could not be read in full") {
		t.Fatalf("both notes: %q", list.Content[max(0, len(list.Content)-200):])
	}
}

// A directory that fails part-way is never presented as complete: what was
// read is listed, and a note says the listing is incomplete.
func TestWorkbenchListSaysWhenADirectoryFailed(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	for i := 0; i < 600; i++ {
		writeFile(t, filepath.Join(work, "big", fmt.Sprintf("%03d", i)), "")
	}
	writeFile(t, filepath.Join(work, "tree", "ok", "kept.txt"), "")
	writeFile(t, filepath.Join(work, "tree", "bad", "lost.txt"), "")
	injected := errors.New("injected I/O error")
	fail := func(dir string, at int) func(string, int) error {
		return func(native string, batch int) error {
			if filepath.ToSlash(native) == dir && batch == at {
				return injected
			}
			return nil
		}
	}
	const incomplete = "[list_files: 1 directories could not be read in full, so this listing is incomplete]"

	// Part-way through the directory asked for.
	ws.dirFault = fail("big", 1)
	r := call(t, ws, workbenchListFiles, map[string]any{"path": "big"})
	lines := strings.Split(r.Content, "\n")
	if r.IsError || len(lines) != workbenchDirBatch+1 || lines[len(lines)-1] != incomplete {
		t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}

	// Before anything of the directory asked for was read: an error, not an
	// empty listing.
	ws.dirFault = fail("big", 0)
	refusedWith(t, call(t, ws, workbenchListFiles, map[string]any{"path": "big"}), wbUnreadable)

	// A subdirectory: the rest of the tree is listed, and the note says one
	// directory is missing entries.
	ws.dirFault = fail("tree/bad", 0)
	r = call(t, ws, workbenchListFiles, map[string]any{"path": "tree"})
	if r.Content != "tree/bad/\ntree/ok/\ntree/ok/kept.txt\n"+incomplete {
		t.Fatalf("%q", r.Content)
	}

	// A subdirectory that is gone by the time it is opened is reported the
	// same way, not skipped silently.
	ws.dirFault = nil
	ws.listed = func(rel string) {
		// After tree's entries were taken in, before its subdirectories are.
		if rel == "tree" {
			if err := os.RemoveAll(filepath.Join(work, "tree", "bad")); err != nil {
				t.Fatal(err)
			}
		}
	}
	r = call(t, ws, workbenchListFiles, map[string]any{"path": "tree"})
	if r.Content != "tree/bad/\ntree/ok/\ntree/ok/kept.txt\n"+incomplete {
		t.Fatalf("%q", r.Content)
	}
}

// The walk reads names only, so an entry's type never depends on whether the
// file system filled in a directory entry's type: each is typed by Lstat of
// the entry itself. A directory is marked and descended, a link is marked and
// not followed, and a name gone since it was read is left out.
func TestWorkbenchListTypesEachEntryItself(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "dir", "inner.txt"), "")
	symlink(t, "dir", filepath.Join(work, "link"))
	for rel, want := range map[string]struct {
		line          string
		isDir, exists bool
	}{
		"a.txt": {"a.txt", false, true},
		"dir":   {"dir/", true, true},
		"link":  {"link [link]", false, true},
		"gone":  {"", false, false},
	} {
		line, info, exists := describe(ws.root, rel, rel)
		isDir := info != nil && info.IsDir()
		if line != want.line || isDir != want.isDir || exists != want.exists {
			t.Errorf("%s: %q %v %v", rel, line, isDir, exists)
		}
	}
	if r := call(t, ws, workbenchListFiles, map[string]any{}); r.Content != "a.txt\ndir/\ndir/inner.txt\nlink [link]" {
		t.Fatalf("%q", r.Content)
	}
}

// Every name read counts towards the walk's bound, hidden ones included, and
// names are read a batch at a time: a directory of reserved temporaries is
// neither read whole nor walked without end.
func TestWorkbenchListWalkIsBounded(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	for i := 0; i < 1000; i++ {
		writeFile(t, filepath.Join(work, "hidden", fmt.Sprintf(".harness-workbench-%04d.tmp", i)), "")
	}
	writeFile(t, filepath.Join(work, "hidden", "visible.txt"), "")
	ws.maxVisited = 300
	batches := 0
	ws.dirFault = func(_ string, batch int) error {
		batches = max(batches, batch+1)
		return nil
	}
	r := call(t, ws, workbenchListFiles, map[string]any{"path": "hidden"})
	if !strings.HasSuffix(r.Content, "the listing stopped after 300 names, so more were left out. Narrow it with path or depth]") || strings.Contains(r.Content, ".harness-workbench") {
		t.Fatalf("%q", r.Content)
	}
	if batches > 300/workbenchDirBatch+1 {
		t.Fatalf("read %d batches of a directory the bound stops in the second", batches)
	}
}

// Cancellation is noticed while a batch's names are processed, not only
// between batches.
func TestWorkbenchListCancelsWithinABatch(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	for i := 0; i < 2*workbenchDirBatch; i++ {
		writeFile(t, filepath.Join(work, "dir", strconv.Itoa(i)), "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps, batches := 0, 0
	ws.step = func() {
		// The first step is the directory's own; the next come every
		// listCheckEvery names of its first batch.
		if steps++; steps == 3 {
			cancel()
		}
	}
	ws.dirFault = func(_ string, batch int) error {
		batches = max(batches, batch+1)
		return nil
	}
	_, err := ws.listFiles(ctx, json.RawMessage(`{"path":"dir"}`))
	if !errors.Is(err, context.Canceled) || batches != 1 || ws.handles.Load() != 0 {
		t.Fatalf("%v after %d batches, %d handles open", err, batches, ws.handles.Load())
	}
}

func TestWorkbenchListBounds(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	for i := 0; i < 2500; i++ {
		writeFile(t, filepath.Join(work, "many", "f"+strconv.Itoa(10000+i)), "")
	}
	r := call(t, ws, workbenchListFiles, map[string]any{"path": "many"})
	entries := strings.Split(r.Content, "\n")
	if len(entries) != 2001 || entries[0] != "many/f10000" || entries[2000] != "[list_files: 2000 entries shown; 500 omitted. Narrow it with path or depth]" {
		t.Fatalf("%d lines, first %q, last %q", len(entries), entries[0], entries[len(entries)-1])
	}

	deep := work
	for i := 1; i <= 10; i++ {
		deep = filepath.Join(deep, "d"+strconv.Itoa(i))
	}
	writeFile(t, filepath.Join(deep, "bottom.txt"), "")
	r = call(t, ws, workbenchListFiles, map[string]any{"path": "d1", "depth": 50})
	if !strings.HasSuffix(r.Content, "\nd1/d2/d3/d4/d5/d6/d7/d8/d9/") || strings.Contains(r.Content, "d9/d10") {
		t.Fatalf("depth was not clamped to 8:\n%s", r.Content)
	}
	if r = call(t, ws, workbenchListFiles, map[string]any{"path": "d1"}); r.Content != "d1/d2/\nd1/d2/d3/" {
		t.Fatalf("default depth: %q", r.Content)
	}

	writeFile(t, filepath.Join(work, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(work, "src", ".git"), "gitdir: elsewhere\n")
	reserved := ".harness-workbench-0123456789abcdef0123456789abcdef-0123456789abcdef.tmp"
	writeFile(t, filepath.Join(work, "src", reserved), "half written\n")
	writeFile(t, filepath.Join(work, "src", "main.go"), "package main\n")
	top := call(t, ws, workbenchListFiles, map[string]any{"depth": 1})
	r = call(t, ws, workbenchListFiles, map[string]any{"path": "src"})
	if strings.Contains(top.Content, ".git") || !strings.Contains(top.Content, "src/") || r.Content != "src/main.go" {
		t.Fatalf("listing:\n%s\n%s", top.Content, r.Content)
	}
	if r = read(t, ws, ".git/HEAD"); r.IsError && ToolAvailability(workbenchReadFile) == "" {
		t.Fatalf(".git/HEAD is readable: %+v", r)
	}
	code := wbReserved
	if ToolAvailability(workbenchReadFile) != "" {
		code = RefusedNotOffered
	}
	refusedWith(t, read(t, ws, "src/"+reserved), code)
	refusedWith(t, read(t, ws, "src/.HARNESS-WORKBENCH-other.TMP"), code)

	empty := filepath.Join(work, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if r = call(t, ws, workbenchListFiles, map[string]any{"path": "empty"}); r.IsError || !strings.Contains(r.Content, "empty") {
		t.Fatalf("%+v", r)
	}
}

// A cancelled read stops at the next chunk, returns context.Canceled and
// holds no handle.
func TestWorkbenchCancelStopsTheRead(t *testing.T) {
	ws, work, _ := testWorkspace(t)
	writeFile(t, filepath.Join(work, "big.txt"), strings.Repeat("line\n", 1<<18))
	for i := 0; i < 600; i++ {
		writeFile(t, filepath.Join(work, "dir", strconv.Itoa(i)), "")
	}
	for _, tc := range []struct {
		tool string
		args string
	}{
		{workbenchReadFile, `{"path":"big.txt"}`},
		{workbenchListFiles, `{"path":"dir"}`},
	} {
		if contentDisabled(t, tc.tool) {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		steps := 0
		ws.step = func() {
			if steps++; steps == 2 {
				cancel()
			}
		}
		var err error
		if tc.tool == workbenchReadFile {
			_, err = ws.readFile(ctx, json.RawMessage(tc.args))
		} else {
			_, err = ws.listFiles(ctx, json.RawMessage(tc.args))
		}
		cancel()
		if !errors.Is(err, context.Canceled) || ws.handles.Load() != 0 {
			t.Fatalf("%s: %v, %d handles open", tc.tool, err, ws.handles.Load())
		}
	}
}
