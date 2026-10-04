package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// listFiles answers list_files: a directory's entries to a depth, sorted,
// shallowest first when it has to cut. Links are listed and never followed;
// .git and the reserved temporary names are left out.
func (w *Workspace) listFiles(ctx context.Context, raw json.RawMessage) (Result, error) {
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
			return Result{}, err
		}
		seen := 0
		handle, err := c.to(d.node)
		var names []string
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
				names = append(names, name)
				return true, nil
			})
		}
		// A directory's names are taken in order, so what a cut keeps never
		// depends on the order the filesystem returned them in.
		slices.Sort(names)
		for i, name := range names {
			if (i+1)%listCheckEvery == 0 {
				if err := w.checkpoint(ctx); err != nil {
					return Result{}, err
				}
			}
			if hiddenName(name) {
				continue
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
				continue
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
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if w.listed != nil {
			w.listed(d.node.rel)
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
		return Result{Content: "[list_files: the directory is empty]"}, nil
	}
	return Result{Content: strings.Join(append(entries, notes...), "\n")}, nil
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

// fileOtherMount checks regular listing entries too: bind mounts can target
// a file. No content is read and a swapped special file is opened nonblocking.
func (w *Workspace) fileOtherMount(dir *os.Root, name string, info fs.FileInfo) bool {
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
