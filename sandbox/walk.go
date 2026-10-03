package sandbox

// How the workbench's tools reach a directory without ever trusting a name
// they checked earlier. A path is walked one component at a time, each
// directory opened through its parent's handle, never through a longer path
// from the root: so a component swapped for a link after it was checked
// cannot redirect the open, because nothing opens that component by a path
// again. Each directory opened must be the one Lstat saw (os.SameFile), and
// must not be hidden by the tool's policy, judged by its spelling, by its
// identity against its parent's .git, and on Windows by the name the file
// system itself gives the opened handle, which an 8.3 short name cannot
// disguise.

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// A walk's refusals of a directory it opened.
var (
	// errHidden: the directory is one the tool's policy hides, under
	// whatever name it was reached.
	errHidden     = errors.New("directory hidden")
	errOtherMount = errors.New("other mount")
	errLinked     = errors.New("linked file")
	errNotRegular = errors.New("not regular")
	// errChanged: the directory is not the one Lstat saw.
	errChanged = errors.New("directory changed while walked")
)

// walkPolicy is what a tool hides. list_files hides .git and the reserved
// temporaries; read_file only the reserved temporaries, because a reviewer
// may read .git/HEAD.
type walkPolicy struct {
	hides func(name string) bool
	// git also hides whatever is the same directory as its parent's .git.
	git bool
}

var (
	listPolicy = walkPolicy{hides: hiddenName, git: true}
	readPolicy = walkPolicy{hides: wsfile.Reserved}
)

// hiddenName reports a name list_files leaves out: .git in any case, and
// the reserved temporaries. Trailing dots and spaces are ignored, as Win32
// ignores them.
func hiddenName(name string) bool {
	return strings.EqualFold(strings.TrimRight(name, " ."), ".git") || wsfile.Reserved(name)
}

// dirNode is a directory a walk has checked: its name in its parent, what
// Lstat saw of it, and its parent. The workspace's root has no parent.
type dirNode struct {
	name   string
	info   fs.FileInfo
	parent *dirNode
	// rel is its slash path from the root, "" for the root.
	rel string
}

func (n *dirNode) child(name string, info fs.FileInfo) *dirNode {
	return &dirNode{name: name, info: info, parent: n, rel: path.Join(n.rel, name)}
}

func (w *Workspace) rootNode() (*dirNode, error) {
	info, err := w.root.Stat(".")
	if err != nil {
		return nil, err
	}
	return &dirNode{info: info}, nil
}

// cursor holds the opened directories along one path from the root. Moving
// it to another directory keeps the prefix the two paths share, so a
// breadth-first listing opens about one directory per directory listed. It
// holds at most one handle per level.
type cursor struct {
	created []*os.Root // write-created directories remain private until commit
	w       *Workspace
	policy  walkPolicy
	nodes   []*dirNode
	roots   []*os.Root
}

func (w *Workspace) newCursor(root *dirNode, policy walkPolicy) *cursor {
	return &cursor{w: w, policy: policy, nodes: []*dirNode{root}, roots: []*os.Root{w.root}}
}

// to opens the directories from the root to n, reusing those already open,
// and returns n's handle.
func (c *cursor) to(n *dirNode) (*os.Root, error) {
	var chain []*dirNode
	for m := n; m != nil; m = m.parent {
		chain = append(chain, m)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if chain[0] != c.nodes[0] {
		return nil, errChanged
	}
	keep := 1
	for keep < len(chain) && keep < len(c.nodes) && chain[keep] == c.nodes[keep] {
		keep++
	}
	c.truncate(keep)
	for _, m := range chain[keep:] {
		r, err := c.enter(c.roots[len(c.roots)-1], m)
		if err != nil {
			return nil, err
		}
		c.nodes, c.roots = append(c.nodes, m), append(c.roots, r)
	}
	return c.roots[len(c.roots)-1], nil
}

func (c *cursor) truncate(n int) {
	for len(c.roots) > n {
		_ = c.roots[len(c.roots)-1].Close()
		c.w.handles.Add(-1)
		c.nodes, c.roots = c.nodes[:len(c.nodes)-1], c.roots[:len(c.roots)-1]
	}
}

// close gives up every handle but the workspace's own root.
func (c *cursor) close() { c.truncate(1) }

// enter opens n through its parent's handle and checks it: still a
// directory rather than a link, the same one Lstat saw, and not hidden.
func (c *cursor) enter(parent *os.Root, n *dirNode) (*os.Root, error) {
	if c.policy.hides(n.name) {
		return nil, errHidden
	}
	info, err := parent.Lstat(n.name)
	switch {
	case err != nil:
		return nil, err
	case info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || !os.SameFile(info, n.info):
		return nil, errChanged
	}
	if c.policy.git {
		if git, err := parent.Lstat(".git"); err == nil && os.SameFile(git, info) {
			return nil, errHidden
		}
	}
	// A trailing dot forces the child to be an intermediate directory open.
	// Go OpenRoot otherwise opens its final component without O_DIRECTORY,
	// which can block on a swapped-in FIFO. The dot uses that same handle.
	if c.w.openStep != nil {
		c.w.openStep()
	}
	r, err := parent.OpenRoot(n.name + "/.")
	if err != nil {
		return nil, err
	}
	c.w.handles.Add(1)
	release := func() {
		_ = r.Close()
		c.w.handles.Add(-1)
	}
	f, err := r.OpenFile(".", wsfile.DirectoryFlags, 0)
	if err != nil {
		release()
		return nil, err
	}
	facts, err := wsfile.Check(f, c.w.mount)
	f.Close()
	if err != nil {
		release()
		return nil, err
	}
	if !facts.Directory {
		release()
		return nil, errChanged
	}
	if !facts.SameMount {
		release()
		return nil, errOtherMount
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(opened, n.info) {
		release()
		return nil, errChanged
	}
	if aliasedNames {
		if hidden, err := c.hiddenByRealName(r); err != nil || hidden {
			release()
			if err != nil {
				return nil, err
			}
			return nil, errHidden
		}
	}
	return r, nil
}

// hiddenByRealName judges an opened directory by the name the file system
// gives it, not the one it was reached by.
func (c *cursor) hiddenByRealName(r *os.Root) (bool, error) {
	f, err := r.Open(".")
	if err != nil {
		return false, err
	}
	defer f.Close()
	name, err := realName(f)
	if err != nil {
		return false, err
	}
	return c.policy.hides(name), nil
}
