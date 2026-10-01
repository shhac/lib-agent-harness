package session

// The workbench's read tools. The model's path is first cleaned by the rules
// wsfile shares with skills, and an element Win32 reads as a device is
// refused on every platform. Then it is walked a component at a time through
// directory handles (workbench_walk.go), all inside one os.Root on the
// resolved WorkDir, so no .., absolute path or link reaches outside, and no
// name checked earlier is trusted when something is opened. What is read is
// decided by the opened handle.
//
// Results are text the library builds: file contents, entry names, fixed
// codes and the relative path the model gave. No host path or OS error prose
// is ever included.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// Workbench tool bounds.
const (
	// maxWorkbenchResult is a result's own limit, before the host's.
	maxWorkbenchResult = 64 << 10
	// minWorkbenchResult is the least MaxResultBytes a workbench accepts: room
	// for some content, its closing note, and any error with its quoted path.
	minWorkbenchResult = 4 << 10
	// maxQuotedPath bounds, in bytes (cutRunes counts bytes), the path an
	// error repeats before quoting. strconv.Quote writes at most four bytes per
	// input byte: a one-byte control or invalid byte becomes \xNN, a two-byte
	// C1 control becomes \uNNNN (three per byte), and anything wider less. So a
	// quoted path is at most 4*256+2 bytes, well inside minWorkbenchResult.
	maxQuotedPath    = 256
	maxReadLines     = 2000
	maxReadFileBytes = 16 << 20
	maxListEntries   = 2000
	defaultListDepth = 2
	maxListDepth     = 8
	// maxListVisited bounds the names one list_files call reads, hidden ones
	// included; past it the listing says it stopped.
	maxListVisited = 20000
	// listCheckEvery is how many names pass between checkpoints; cancellation
	// itself is checked at every name.
	listCheckEvery    = 64
	maxWorkbenchPath  = 1024
	workbenchDirBatch = 256
	// noteRoom is kept free in a result for its closing note.
	noteRoom = 256
	// maxLinkHops bounds the links read_file follows for one path, as the
	// operating systems bound theirs.
	maxLinkHops = 40
)

// Result codes the model sees.
const (
	wbArgumentsInvalid = "arguments_invalid"
	wbPathInvalid      = "file_path_invalid"
	wbOutside          = "file_outside_workspace"
	wbNotFound         = "file_not_found"
	wbNotRegular       = "file_not_regular"
	wbNotDirectory     = "file_not_directory"
	wbTooLarge         = "file_too_large"
	wbNotText          = "file_not_text"
	wbReserved         = "file_reserved"
	wbUnreadable       = "file_unreadable"
	wbLinked           = "file_linked"
	wbOtherMount       = "file_other_mount"
	// list_files never follows a link, not even one it was asked for.
	wbIsSymlink      = "file_is_symlink"
	wbThroughSymlink = "path_through_symlink"
	// list_files does not enumerate .git or a reserved temporary name, asked
	// for directly, reached on the way, or reached under another name.
	wbNotListed = "path_not_listed"
)

// workspace is one session's handle on its WorkDir.
type workspace struct {
	id         string
	mode       fs.FileMode
	writeFault func(string) error
	commands   *workbenchRunner
	root       *os.Root
	mount      wsfile.Mount
	jobs       chan workspaceJob
	stop       chan struct{}
	stopped    chan struct{}
	stopOnce   sync.Once
	stuck      atomic.Pointer[TurnError]
	failed     func(error)
	grace      time.Duration
	// budget is the smaller of a tool's own limit and the host's
	// MaxResultBytes, so the host never cuts a result without it saying so.
	budget int
	// handles counts files and directories the tools hold open beyond the
	// root, for tests.
	handles atomic.Int32
	// step, when set, runs before each chunk or directory batch; tests use it
	// to act mid-call.
	step func()
	// openStep lets tests swap a name after Lstat but before an open.
	openStep func()
	// dirFault, when set, fails reading a directory before its batch n; tests
	// use it to stand in for an I/O error part-way through a listing. rel is
	// the directory's slash path from the root.
	dirFault func(rel string, batch int) error
	// maxVisited, when set, replaces maxListVisited; tests use it.
	maxVisited int
}

// openWorkspace opens the session's root, or returns nil for a session
// without a workbench.
func openWorkspace(o Options) (*workspace, error) {
	if o.Workbench == nil {
		return nil, nil
	}
	if !wsfile.MountCheckSupported {
		return nil, refuse(o, "workbench", RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity")
	}
	root, err := os.OpenRoot(o.WorkDir + string(filepath.Separator) + ".")
	if err != nil {
		return nil, refuse(o, "work_dir", RefusedWorkDir, "the workspace could not be opened")
	}
	f, err := root.OpenFile(".", wsfile.DirectoryFlags, 0)
	if err != nil {
		root.Close()
		return nil, refuse(o, "workbench", RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity")
	}
	mount, err := workspaceMountID(f)
	f.Close()
	if err != nil {
		root.Close()
		return nil, refuse(o, "workbench", RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity")
	}
	budget := maxWorkbenchResult
	if limit := o.Restriction.Tools.MaxResultBytes; limit > 0 && limit < budget {
		budget = limit
	}
	w := &workspace{mode: o.Workbench.NewFileMode, root: root, mount: mount, budget: budget, grace: workspaceGrace, jobs: make(chan workspaceJob), stop: make(chan struct{}), stopped: make(chan struct{})}
	go w.worker()
	return w, nil
}

func (w *workspace) close() {
	if w != nil {
		w.stopOnce.Do(func() { close(w.stop); _ = w.root.Close() })
	}
}

// openIn opens a file through a directory handle, counted.
func (w *workspace) openIn(dir *os.Root, name string) (*os.File, error) {
	if w.openStep != nil {
		w.openStep()
	}
	f, err := dir.OpenFile(name, wsfile.OpenFlags, 0)
	if err != nil {
		return nil, err
	}
	facts, err := wsfile.Check(f, w.mount)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !facts.Regular && !facts.Directory {
		f.Close()
		return nil, errNotRegular
	}
	if facts.Regular && facts.Links != 1 {
		f.Close()
		return nil, errLinked
	}
	if !facts.SameMount {
		f.Close()
		return nil, errOtherMount
	}
	w.handles.Add(1)
	return f, nil
}

func (w *workspace) release(f *os.File) {
	_ = f.Close()
	w.handles.Add(-1)
}

// checkpoint stops a call between steps once its context ends.
func (w *workspace) checkpoint(ctx context.Context) error {
	if w.step != nil {
		w.step()
	}
	return ctx.Err()
}

func workbenchError(tool, code, rel string) ToolResult {
	text := tool + " error: " + code
	if rel != "" {
		text += ": " + strconv.Quote(cutRunes(rel, maxQuotedPath))
	}
	return ToolResult{Content: text, IsError: true}
}

// resolveName cleans the model's path and returns it, or the code refusing
// it. An empty path or "." names the root itself where root is allowed.
func resolveName(rel string, root bool) (clean, code string) {
	if root && (rel == "" || rel == ".") {
		return ".", ""
	}
	clean, problem := wsfile.Clean(rel, maxWorkbenchPath)
	switch {
	case problem == wsfile.PathOutside:
		return "", wbOutside
	case problem != wsfile.PathOK, wsfile.ReservedDevice(clean):
		return "", wbPathInvalid
	}
	return clean, ""
}

// rootFailure names what a walk or the root refused, without its prose.
func rootFailure(err error) string {
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, errOtherMount):
		return wbOtherMount
	case errors.Is(err, errLinked):
		return wbLinked
	case errors.Is(err, errNotRegular):
		return wbNotRegular
	case errors.As(err, &pathErr) && wsfile.NotRegular(pathErr.Err):
		return wbNotRegular
	case errors.Is(err, fs.ErrNotExist):
		return wbNotFound
	case errors.As(err, &pathErr) && pathErr.Err.Error() == "path escapes from parent":
		// os.Root's own refusal of a name, or a link, that leaves the root.
		return wbOutside
	}
	return wbUnreadable
}

// readFile answers read_file: a regular UTF-8 file of at most 16 MiB, from
// line offset, at most limit lines and the result budget.
func (w *workspace) readFile(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return workbenchError(workbenchReadFile, wbArgumentsInvalid, ""), nil
	}
	offset, limit := 1, maxReadLines
	if in.Offset != nil {
		offset = *in.Offset
	}
	if in.Limit != nil {
		limit = min(*in.Limit, maxReadLines)
	}
	if offset < 1 || limit < 1 {
		return workbenchError(workbenchReadFile, wbArgumentsInvalid, ""), nil
	}
	clean, code := resolveName(in.Path, false)
	if code != "" {
		return workbenchError(workbenchReadFile, code, in.Path), nil
	}
	if err := w.checkpoint(ctx); err != nil {
		return ToolResult{}, err
	}
	top, err := w.rootNode()
	if err != nil {
		return workbenchError(workbenchReadFile, wbUnreadable, clean), nil
	}
	c := w.newCursor(top, readPolicy)
	defer c.close()
	f, code := w.openRegular(c, top, clean)
	if code != "" {
		return workbenchError(workbenchReadFile, code, clean), nil
	}
	defer w.release(f)
	opened, err := f.Stat()
	switch {
	case err != nil:
		return workbenchError(workbenchReadFile, wbUnreadable, clean), nil
	case opened.Size() > maxReadFileBytes:
		return workbenchError(workbenchReadFile, wbTooLarge, clean), nil
	}
	data, tooLarge, err := wsfile.ReadAtMost(f, maxReadFileBytes, func() error { return w.checkpoint(ctx) })
	switch {
	case ctx.Err() != nil:
		return ToolResult{}, ctx.Err()
	case err != nil:
		return workbenchError(workbenchReadFile, wbUnreadable, clean), nil
	case tooLarge:
		return workbenchError(workbenchReadFile, wbTooLarge, clean), nil
	case !wsfile.Text(data):
		return workbenchError(workbenchReadFile, wbNotText, clean), nil
	}
	return ToolResult{Content: readLines(data, offset, limit, w.budget)}, nil
}

// openRegular walks clean from the root and opens the regular file it names,
// or returns the code refusing it. It follows links that stay inside the
// workspace, as the design says reads do, but resolves each one itself: a
// link is read through its directory's handle and its target walked again
// from the root, component by component, so every hop meets the same checks.
// A reserved temporary name is refused in any component, of the path asked
// for or of any link's target, and, on Windows, as the file system names what
// was opened.
func (w *workspace) openRegular(c *cursor, top *dirNode, clean string) (*os.File, string) {
	parts := strings.Split(clean, "/")
	node, hops := top, 0
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if wsfile.Reserved(part) {
			return nil, wbReserved
		}
		dir, err := c.to(node)
		switch {
		case errors.Is(err, errHidden):
			return nil, wbReserved
		case err != nil:
			return nil, rootFailure(err)
		}
		info, err := dir.Lstat(part)
		if err != nil {
			return nil, rootFailure(err)
		}
		last := i == len(parts)-1
		switch mode := info.Mode(); {
		case mode&fs.ModeSymlink != 0:
			if c.policy.git {
				return nil, wbIsSymlink
			}
			if hops++; hops > maxLinkHops {
				return nil, wbUnreadable
			}
			target, err := dir.Readlink(part)
			if err != nil {
				return nil, wbUnreadable
			}
			next, code := linkTarget(node.rel, target, parts[i+1:])
			if code != "" {
				return nil, code
			}
			if next == "." {
				return nil, wbNotRegular
			}
			parts, node, i = strings.Split(next, "/"), top, -1
		case !last && mode.IsDir():
			node = node.child(part, info)
		case !last:
			return nil, wbNotFound
		case !mode.IsRegular():
			return nil, wbNotRegular
		default:
			if wsfile.LinkCount(info) > 1 {
				return nil, wbLinked
			}
			f, err := w.openIn(dir, part)
			if err != nil {
				return nil, rootFailure(err)
			}
			opened, err := f.Stat()
			if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, info) {
				// Not the file Lstat saw: something swapped it since.
				w.release(f)
				return nil, wbUnreadable
			}
			if aliasedNames {
				if name, err := realName(f); err != nil || c.policy.hides(name) {
					w.release(f)
					if err != nil {
						return nil, wbUnreadable
					}
					return nil, wbReserved
				}
			}
			return f, ""
		}
	}
	return nil, wbNotFound
}

// linkTarget is the workspace path a link in dir points to, followed by the
// rest of the path being walked, or the code refusing it. A target must be
// relative and stay inside; it is judged as model input is.
func linkTarget(dir, target string, rest []string) (string, string) {
	slash := filepath.ToSlash(target)
	if target == "" || filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(slash, "/") {
		return "", wbOutside
	}
	next := path.Join(append([]string{dir, slash}, rest...)...)
	switch {
	case next == "..", strings.HasPrefix(next, "../"):
		return "", wbOutside
	case next == ".":
		return ".", ""
	}
	if clean, problem := wsfile.Clean(next, maxWorkbenchPath*4); problem != wsfile.PathOK || clean != next || wsfile.ReservedDevice(next) {
		return "", wbPathInvalid
	}
	return next, ""
}

// readLines is lines offset to offset+limit-1 of data, as many as fit in
// budget, with a note saying where to continue when any were left out. It
// scans for line ends in place: only the part shown is copied, so a file of
// millions of short lines costs no more than one of a few long ones.
func readLines(data []byte, offset, limit, budget int) string {
	total := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	if total == 0 {
		return "[read_file: the file is empty]"
	}
	if offset > total {
		return "[read_file: the file has " + strconv.Itoa(total) + " lines; offset " + strconv.Itoa(offset) + " is past its end]"
	}
	// lineEnd is the end of the line starting at i, its newline included.
	lineEnd := func(i int) int {
		if n := bytes.IndexByte(data[i:], '\n'); n >= 0 {
			return i + n + 1
		}
		return len(data)
	}
	start := 0
	for line := 1; line < offset; line++ {
		start = lineEnd(start)
	}
	room := max(budget-noteRoom, budget/2)
	end, last := start, offset-1
	for last < total && last-offset+1 < limit {
		next := lineEnd(end)
		if next-start > room {
			if end == start {
				// One line longer than a result: show its start rather than
				// nothing, and move past it.
				shown := cutRunes(string(data[start:min(next, start+room+utf8.UTFMax)]), room)
				return shown + "\n[read_file: line " + strconv.Itoa(offset) + " is longer than one result; only its first " + strconv.Itoa(len(shown)) + " bytes are shown. Continue with offset " + strconv.Itoa(offset+1) + "]"
			}
			break
		}
		end = next
		last++
	}
	body := string(data[start:end])
	if last == total {
		return body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + "[read_file: lines " + strconv.Itoa(offset) + "-" + strconv.Itoa(last) + " of " + strconv.Itoa(total) + " shown. Continue with offset " + strconv.Itoa(last+1) + "]"
}

// listFiles answers list_files: a directory's entries to a depth, sorted,
// shallowest first when it has to cut. Links are listed and never followed;
// .git and the reserved temporary names are left out.
func (w *workspace) listFiles(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Path  string `json:"path"`
		Depth *int   `json:"depth"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return workbenchError(workbenchListFiles, wbArgumentsInvalid, ""), nil
	}
	depth := defaultListDepth
	if in.Depth != nil {
		depth = min(max(*in.Depth, 1), maxListDepth)
	}
	clean, code := resolveName(in.Path, true)
	if code != "" {
		return workbenchError(workbenchListFiles, code, in.Path), nil
	}
	shown := clean
	if clean == "." {
		shown = ""
	}
	root, err := w.rootNode()
	if err != nil {
		return workbenchError(workbenchListFiles, wbUnreadable, shown), nil
	}
	c := w.newCursor(root, listPolicy)
	defer c.close()
	top, code := w.listTarget(c, root, clean)
	if code != "" {
		return workbenchError(workbenchListFiles, code, shown), nil
	}
	type dir struct {
		node  *dirNode
		level int
	}
	var (
		entries []string
		used    int
		omitted int
		// visited counts every name read, hidden ones included, so the walk
		// is bounded however a tree is made up. capped says it stopped there.
		visited int
		capped  bool
		limit   = w.visitLimit()
		// incomplete counts directories whose entries could not all be read.
		incomplete int
		room       = max(w.budget-noteRoom, w.budget/2)
		queue      = []dir{{top, 1}}
	)
	for len(queue) > 0 && !capped {
		d := queue[0]
		queue = queue[1:]
		if err := w.checkpoint(ctx); err != nil {
			return ToolResult{}, err
		}
		seen := 0
		handle, err := c.to(d.node)
		if err == nil {
			err = w.eachName(ctx, handle, d.node.rel, func(name string) (bool, error) {
				if visited >= limit {
					capped = true
					return false, nil
				}
				visited++
				seen++
				if visited%listCheckEvery == 0 {
					if err := w.checkpoint(ctx); err != nil {
						return false, err
					}
				} else if err := ctx.Err(); err != nil {
					return false, err
				}
				if hiddenName(name) {
					return true, nil
				}
				rel := path.Join(d.node.rel, name)
				line, info, exists := describe(handle, name, rel)
				if info != nil && info.Mode().IsRegular() && w.fileOtherMount(handle, name, info) {
					line = rel + " [mount]"
					info = nil
				}
				if info != nil && info.IsDir() {
					probe := d.node.child(name, info)
					r, e := c.enter(handle, probe)
					if e == nil {
						r.Close()
						w.handles.Add(-1)
					}
					if errors.Is(e, errOtherMount) {
						line = rel + "/ [mount]"
						info = nil
					}
				}
				if !exists {
					// Gone since the directory was read: nothing to list.
					return true, nil
				}
				if info != nil && info.IsDir() && d.level < depth {
					queue = append(queue, dir{d.node.child(name, info), d.level + 1})
				}
				if len(entries) < maxListEntries && used+len(line)+1 <= room {
					entries = append(entries, line)
					used += len(line) + 1
				} else {
					omitted++
				}
				return true, nil
			})
		}
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		switch {
		case errors.Is(err, errHidden) && d.level == 1:
			return workbenchError(workbenchListFiles, wbNotListed, shown), nil
		case errors.Is(err, errHidden):
			// Hidden under another name: left out, as by name.
		case err != nil:
			if d.level == 1 && seen == 0 {
				return workbenchError(workbenchListFiles, rootFailure(err), shown), nil
			}
			// What was read is listed, and the note says the listing is
			// incomplete: a directory that went away, cannot be opened, was
			// swapped, or failed part-way through.
			incomplete++
		}
	}
	slices.Sort(entries)
	var notes []string
	switch {
	case capped:
		notes = append(notes, "[list_files: "+strconv.Itoa(len(entries))+" entries shown; the listing stopped after "+strconv.Itoa(visited)+" names, so more were left out. Narrow it with path or depth]")
	case omitted > 0:
		notes = append(notes, "[list_files: "+strconv.Itoa(len(entries))+" entries shown; "+strconv.Itoa(omitted)+" omitted. Narrow it with path or depth]")
	}
	if incomplete > 0 {
		notes = append(notes, "[list_files: "+strconv.Itoa(incomplete)+" directories could not be read in full, so this listing is incomplete]")
	}
	if len(entries) == 0 && len(notes) == 0 {
		return ToolResult{Content: "[list_files: the directory is empty]"}, nil
	}
	return ToolResult{Content: strings.Join(append(entries, notes...), "\n")}, nil
}

// listTarget walks to the directory list_files was asked for, one component
// at a time through the cursor, following no link: a link is refused as the
// last component or on the way, and a hidden component, by spelling or by
// what it opens, is refused. It returns the directory's node.
func (w *workspace) listTarget(c *cursor, root *dirNode, clean string) (*dirNode, string) {
	if clean == "." {
		return root, ""
	}
	parts := strings.Split(clean, "/")
	node := root
	for i, part := range parts {
		if hiddenName(part) {
			return nil, wbNotListed
		}
		dir, err := c.to(node)
		switch {
		case errors.Is(err, errHidden):
			return nil, wbNotListed
		case err != nil:
			return nil, rootFailure(err)
		}
		info, err := dir.Lstat(part)
		if err != nil {
			return nil, rootFailure(err)
		}
		switch last := i == len(parts)-1; {
		case info.Mode()&fs.ModeSymlink != 0 && last:
			return nil, wbIsSymlink
		case info.Mode()&fs.ModeSymlink != 0:
			return nil, wbThroughSymlink
		case !info.IsDir():
			return nil, wbNotDirectory
		}
		node = node.child(part, info)
	}
	// Open it now, so a hidden directory is refused as asked for rather than
	// as an empty listing.
	if _, err := c.to(node); err != nil {
		if errors.Is(err, errHidden) {
			return nil, wbNotListed
		}
		return nil, rootFailure(err)
	}
	return node, ""
}

// eachName reads the names in an opened directory and hands visit them a
// batch at a time, so no more than one batch is ever held. visit returns
// false to stop. A failure part-way returns the error after the names
// already visited, so the caller can say the listing is incomplete. Only
// names are read: what each one is comes from describe, never from the type
// a directory entry may or may not carry.
func (w *workspace) eachName(ctx context.Context, dir *os.Root, rel string, visit func(name string) (bool, error)) error {
	f, err := w.openIn(dir, ".")
	if err != nil {
		return err
	}
	defer w.release(f)
	for n := 0; ; n++ {
		if w.dirFault != nil {
			if err := w.dirFault(rel, n); err != nil {
				return err
			}
		}
		names, err := f.Readdirnames(workbenchDirBatch)
		for _, name := range names {
			more, verr := visit(name)
			if verr != nil {
				return verr
			}
			if !more {
				return nil
			}
		}
		switch {
		case errors.Is(err, io.EOF), err == nil && len(names) == 0:
			return nil
		case err != nil:
			return err
		}
		if err := w.checkpoint(ctx); err != nil {
			return err
		}
	}
}

// describe is an entry's listing line, from Lstat through its directory's
// handle: the entry itself, never what a link names. info is that Lstat, nil
// when it failed, and exists is false for a name that went away after it was
// read. Descending opens the directory through the same handle and checks it
// against info (cursor.enter).
func describe(dir *os.Root, name, rel string) (line string, info fs.FileInfo, exists bool) {
	info, err := dir.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil, false
	case err != nil:
		return rel + " [unreadable]", nil, true
	}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return rel + " [link]", info, true
	case mode.IsDir():
		return rel + "/", info, true
	case mode&fs.ModeNamedPipe != 0:
		return rel + " [fifo]", info, true
	case !mode.IsRegular():
		return rel + " [other]", info, true
	}
	if wsfile.LinkCount(info) > 1 {
		return rel + " [linked]", info, true
	}
	return rel, info, true
}

func (w *workspace) visitLimit() int {
	if w.maxVisited > 0 {
		return w.maxVisited
	}
	return maxListVisited
}

// fileOtherMount checks regular listing entries too: bind mounts can target
// a file. No content is read and a swapped special file is opened nonblocking.
func (w *workspace) fileOtherMount(dir *os.Root, name string, info fs.FileInfo) bool {
	f, err := dir.OpenFile(name, wsfile.OpenFlags, 0)
	if err != nil {
		return false
	}
	w.handles.Add(1)
	defer w.release(f)
	opened, err := f.Stat()
	if err != nil || !os.SameFile(opened, info) {
		return false
	}
	facts, err := wsfile.Check(f, w.mount)
	return err == nil && facts.Regular && !facts.SameMount
}
