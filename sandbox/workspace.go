package sandbox

// The workbench's read tools. The model's path is first cleaned by the rules
// wsfile shares with skills, and an element Win32 reads as a device is
// refused on every platform. Then it is walked a component at a time through
// directory handles (walk.go), all inside one os.Root on the
// resolved WorkDir, so no .., absolute path or link reaches outside, and no
// name checked earlier is trusted when something is opened. These independent
// observations do not prove coherent admission under concurrent unlink.
// Content operations are disabled on Linux/macOS until admission is verified.
//
// Results are text the library builds: file contents, entry names, fixed
// codes and the relative path the model gave. No host path or OS error prose
// is ever included.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shhac/lib-agent-harness/internal/textbound"
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

// Workspace is a confined handle on a caller-selected workspace.
type Workspace struct {
	id         string
	mode       fs.FileMode
	writeFault func(string) error
	root       *os.Root
	mount      wsfile.Mount
	jobs       chan workspaceJob
	stop       chan struct{}
	stopped    chan struct{}
	stopOnce   sync.Once
	stuck      atomic.Pointer[CommandError]
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
	// contentStep observes actual target opens/reads, including edits which do
	// not use openIn. Tests install it before admitting work.
	contentStep func(tool, stage string)
	// Instance-local namespace observations for deterministic admission fixtures.
	statName func(*os.Root, string) (fs.FileInfo, error)
	openName func(*os.Root, string) (*os.File, error)
	// dirFault, when set, fails reading a directory before its batch n; tests
	// use it to stand in for an I/O error part-way through a listing. rel is
	// the directory's slash path from the root.
	dirFault func(rel string, batch int) error
	// listed, when set, runs once list_files has taken in a directory's
	// entries and before it reads any of its subdirectories; tests use it to
	// change the tree in that gap.
	listed func(rel string)
	// maxVisited, when set, replaces maxListVisited; tests use it.
	maxVisited int
}

// OpenWorkspace opens a workspace using the caller's effective permissions and budget.
func OpenWorkspace(config Config) (*Workspace, error) {
	if !wsfile.MountCheckSupported {
		return nil, refusal("workbench", RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity")
	}
	root, err := os.OpenRoot(config.Root + string(filepath.Separator) + ".")
	if err != nil {
		return nil, refusal("work_dir", RefusedWorkDir, "the workspace could not be opened")
	}
	f, err := root.OpenFile(".", wsfile.DirectoryFlags, 0)
	if err != nil {
		root.Close()
		return nil, refusal("workbench", RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity")
	}
	mount, err := workspaceMountID(f)
	f.Close()
	if err != nil {
		root.Close()
		return nil, refusal("workbench", RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity")
	}
	budget := maxWorkbenchResult
	if limit := config.Budget; limit > 0 && limit < budget {
		budget = limit
	}
	w := &Workspace{id: config.SessionID, mode: config.NewFileMode, failed: config.OnFailure, root: root, mount: mount, budget: budget, grace: workspaceGrace, jobs: make(chan workspaceJob), stop: make(chan struct{}), stopped: make(chan struct{})}
	if config.Grace > 0 {
		w.grace = config.Grace
	}
	go w.worker()
	return w, nil
}

func (w *Workspace) close() {
	if w != nil {
		w.stopOnce.Do(func() { close(w.stop); _ = w.root.Close() })
	}
}

// openIn opens a file through a directory handle, counted.
func (w *Workspace) openIn(dir *os.Root, name string) (*os.File, error) {
	if w.openStep != nil {
		w.openStep()
	}
	open := func(dir *os.Root, name string) (*os.File, error) { return dir.OpenFile(name, wsfile.OpenFlags, 0) }
	if w.openName != nil {
		open = w.openName
	}
	f, err := open(dir, name)
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

func (w *Workspace) release(f *os.File) {
	_ = f.Close()
	w.handles.Add(-1)
}

// checkpoint stops a call between steps once its context ends.
func (w *Workspace) checkpoint(ctx context.Context) error {
	if w.step != nil {
		w.step()
	}
	return ctx.Err()
}

func workbenchError(tool, code, rel string) Result {
	text := tool + " error: " + code
	if rel != "" {
		text += ": " + strconv.Quote(textbound.Cut(rel, maxQuotedPath))
	}
	return Result{Content: text, IsError: true}
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
	case errors.Is(err, errHidden):
		// A directory the tool's policy hides, such as .git reached by its
		// Windows short name, is reserved, not merely unreadable.
		return wbReserved
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

// openRegular walks clean from the root and opens the regular file it names,
// or returns the code refusing it. It follows links that stay inside the
// workspace, as the design says reads do, but resolves each one itself: a
// link is read through its directory's handle and its target walked again
// from the root, component by component, so every hop meets the same checks.
// A reserved temporary name is refused in any component, of the path asked
// for or of any link's target, and, on Windows, as the file system names what
// was opened.
func (w *Workspace) openRegular(c *cursor, top *dirNode, clean string) (*os.File, string) {
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
		info, err := w.lstat(dir, part)
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
			// Resample identity after open. Independent namespace and descriptor
			// samples do not prove admission under concurrent unlink; the
			// Linux/macOS content policy refuses before this path.
			if again, err := w.lstat(dir, part); err != nil || !os.SameFile(again, info) || wsfile.LinkCount(again) > 1 {
				w.release(f)
				return nil, wbLinked
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

// listTarget walks to the directory list_files was asked for, one component
// at a time through the cursor, following no link: a link is refused as the
// last component or on the way, and a hidden component, by spelling or by
// what it opens, is refused. It returns the directory's node.
func (w *Workspace) listTarget(c *cursor, root *dirNode, clean string) (*dirNode, string) {
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
func (w *Workspace) eachName(ctx context.Context, dir *os.Root, rel string, visit func(name string) (bool, error)) error {
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

func (w *Workspace) visitLimit() int {
	if w.maxVisited > 0 {
		return w.maxVisited
	}
	return maxListVisited
}

func (w *Workspace) lstat(dir *os.Root, name string) (fs.FileInfo, error) {
	if w.statName != nil {
		return w.statName(dir, name)
	}
	return dir.Lstat(name)
}
