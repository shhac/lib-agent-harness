package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

const wbWriteFailed = "write_failed"
const wbWriteUnknown = "write_outcome_unknown"

var errWorkbenchWriteUnknown = errors.New(wbWriteUnknown)

func workbenchWriteDefinitions() []ToolDefinition {
	properties := func(fields ...string) map[string]any {
		p := map[string]any{}
		for _, f := range fields {
			p[f] = map[string]any{"type": "string"}
		}
		return p
	}
	p := properties("path", "old", "new")
	p["replace_all"] = map[string]any{"type": "boolean"}
	return []ToolDefinition{
		{Name: workbenchWriteFile, Description: "Atomically replace a workspace UTF-8 file (at most 1 MiB), creating parents. Symlinks and .git are refused.", Schema: map[string]any{"type": "object", "properties": properties("path", "content"), "required": []any{"path", "content"}, "additionalProperties": false}},
		{Name: workbenchEditFile, Description: "Replace one exact occurrence of old with new in a workspace UTF-8 file, or all occurrences with replace_all. Symlinks, hard links and .git are refused.", Schema: map[string]any{"type": "object", "properties": p, "required": []any{"path", "old", "new"}, "additionalProperties": false}},
	}
}

// writeParent keeps every ancestor handle until durability is established.
// No multi-component path is opened after a name has been checked.
func (w *workspace) writeParent(ctx context.Context, rel string, create bool) (*cursor, *os.Root, []*os.Root, string) {
	n, err := w.rootNode()
	if err != nil {
		return nil, nil, nil, wbWriteFailed
	}
	c := w.newCursor(n, listPolicy)
	dir := w.root
	var syncParents []*os.Root
	for _, name := range strings.Split(path.Dir(rel), "/") {
		if name == "." {
			continue
		}
		if err := w.checkpoint(ctx); err != nil {
			c.close()
			return nil, nil, nil, wbWriteFailed
		}
		if hiddenName(name) {
			c.close()
			return nil, nil, nil, wbReserved
		}
		info, err := dir.Lstat(name)
		created := false
		if errors.Is(err, fs.ErrNotExist) && create {
			if err = dir.Mkdir(name, 0700); err == nil {
				created = true
				syncParents = append(syncParents, dir)
				info, err = dir.Lstat(name)
			}
		}
		if err != nil {
			c.close()
			return nil, nil, nil, rootFailure(err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			c.close()
			return nil, nil, nil, wbThroughSymlink
		}
		child := n.child(name, info)
		r, err := c.enter(dir, child)
		if err != nil {
			c.close()
			return nil, nil, nil, rootFailure(err)
		}
		c.nodes = append(c.nodes, child)
		c.roots = append(c.roots, r)
		if created {
			c.created = append(c.created, r)
		}
		n, dir = child, r
	}
	return c, dir, syncParents, ""
}

func (w *workspace) writeFile(ctx context.Context, tool string, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
		Old     *string `json:"old"`
		New     *string `json:"new"`
		All     bool    `json:"replace_all"`
	}
	fail := func(code string) (ToolResult, error) {
		r := workbenchError(tool, code, in.Path)
		if code == wbWriteUnknown {
			return r, errWorkbenchWriteUnknown
		}
		return r, nil
	}
	if ctx.Err() != nil {
		return fail(wbWriteFailed)
	}
	if json.Unmarshal(raw, &in) != nil || (tool == workbenchWriteFile && in.Content == nil) || (tool == workbenchEditFile && (in.Old == nil || in.New == nil || *in.Old == "")) {
		return fail(wbArgumentsInvalid)
	}
	if tool == workbenchWriteFile {
		if len(*in.Content) > 1<<20 {
			return fail(wbTooLarge)
		}
		if !utf8.ValidString(*in.Content) || strings.Contains(*in.Content, "\x00") {
			return fail(wbNotText)
		}
	}
	rel, code := resolveName(in.Path, false)
	if code != "" {
		return fail(code)
	}
	for _, name := range strings.Split(rel, "/") {
		if hiddenName(name) {
			return fail(wbReserved)
		}
	}
	c, dir, parents, code := w.writeParent(ctx, rel, tool == workbenchWriteFile)
	if code != "" {
		return fail(code)
	}
	defer c.close()
	name := path.Base(rel)
	mode := w.mode
	if mode == 0 {
		mode = 0600
	}
	info, err := dir.Lstat(name)
	content := ""
	if err == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return fail(wbIsSymlink)
		}
		if !info.Mode().IsRegular() {
			return fail(wbNotRegular)
		}
		if git, e := dir.Lstat(".git"); e == nil && os.SameFile(git, info) {
			return fail(wbReserved)
		}
		f, e := dir.OpenFile(name, wsfile.OpenFlags, 0)
		if e != nil {
			return fail(rootFailure(e))
		}
		defer f.Close()
		facts, e := wsfile.Check(f, w.mount)
		opened, se := f.Stat()
		if e != nil || se != nil || !os.SameFile(info, opened) {
			return fail(wbWriteFailed)
		}
		if !facts.Regular {
			return fail(wbNotRegular)
		}
		if !facts.SameMount {
			return fail(wbOtherMount)
		}
		if opened.Mode().Perm()&0200 == 0 {
			return fail("file_read_only")
		}
		mode = opened.Mode().Perm()
		if tool == workbenchEditFile {
			if facts.Links != 1 {
				return fail(wbLinked)
			}
			data, e := io.ReadAll(io.LimitReader(f, maxReadFileBytes+1))
			if e != nil {
				return fail(wbUnreadable)
			}
			if len(data) > maxReadFileBytes {
				return fail(wbTooLarge)
			}
			if !utf8.Valid(data) || strings.Contains(string(data), "\x00") {
				return fail(wbNotText)
			}
			content = string(data)
		}
		// Do not keep a target handle open across replacement. In particular,
		// Windows rename must not depend on the reader's sharing flags.
		if f.Close() != nil {
			return fail(wbWriteFailed)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(rootFailure(err))
	} else if tool == workbenchEditFile {
		return fail(wbNotFound)
	}
	if tool == workbenchWriteFile {
		content = *in.Content
	} else {
		count := strings.Count(content, *in.Old)
		if count == 0 {
			return fail("match_not_found")
		}
		if count != 1 && !in.All {
			return fail("match_not_unique")
		}
		n := 1
		if in.All {
			n = -1
		}
		matches := 1
		if in.All {
			matches = count
		}
		// Bound expansion before allocating it, including on 32-bit callers.
		length := uint64(len(content)) - uint64(matches)*uint64(len(*in.Old)) + uint64(matches)*uint64(len(*in.New))
		if length > 1<<20 {
			return fail(wbTooLarge)
		}
		content = strings.Replace(content, *in.Old, *in.New, n)
	}
	if len(content) > 1<<20 {
		return fail(wbTooLarge)
	}
	if !utf8.ValidString(content) || strings.Contains(content, "\x00") {
		return fail(wbNotText)
	}
	return w.replaceFile(ctx, tool, rel, dir, name, content, mode, parents, c.created)
}

func (w *workspace) fault(stage string) error {
	if w.writeFault != nil {
		return w.writeFault(stage)
	}
	return nil
}

func (w *workspace) replaceFile(ctx context.Context, tool, rel string, dir *os.Root, name, content string, mode fs.FileMode, parents, created []*os.Root) (ToolResult, error) {
	fail := func(code string) (ToolResult, error) {
		r := workbenchError(tool, code, rel)
		if code == wbWriteUnknown {
			return r, errWorkbenchWriteUnknown
		}
		return r, nil
	}
	if w.checkpoint(ctx) != nil || w.fault("create") != nil {
		return fail(wbWriteFailed)
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fail(wbWriteFailed)
	}
	tmp := ".harness-workbench-" + strings.ReplaceAll(w.id, "-", "") + "-" + hex.EncodeToString(b[:]) + ".tmp"
	f, err := dir.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(wbWriteFailed)
	}
	remove := true
	defer func() {
		f.Close()
		if remove {
			_ = dir.Remove(tmp)
		}
	}()
	if w.fault("write") != nil {
		return fail(wbWriteFailed)
	}
	if _, err = f.WriteString(content); err != nil {
		return fail(wbWriteFailed)
	}
	if w.fault("sync_file") != nil || f.Sync() != nil || w.checkpoint(ctx) != nil {
		return fail(wbWriteFailed)
	}
	if w.fault("chmod") != nil || f.Chmod(workbenchFileMode(mode)) != nil || w.checkpoint(ctx) != nil {
		return fail(wbWriteFailed)
	}
	identity, err := f.Stat()
	if err != nil {
		return fail(wbWriteFailed)
	}
	if f.Close() != nil {
		return fail(wbWriteFailed)
	}
	err = w.fault("rename")
	if err == nil {
		err = dir.Rename(tmp, name)
	}
	if err != nil {
		final, e := dir.Lstat(name)
		if e == nil && os.SameFile(identity, final) {
			err = nil
		} else {
			remaining, e := dir.Lstat(tmp)
			if e == nil && os.SameFile(identity, remaining) {
				return fail(wbWriteFailed)
			}
			remove = false
			return fail(wbWriteUnknown)
		}
	}
	remove = false // Rename committed. Cancellation cannot interrupt durability.
	// Open sync handles before applying potentially non-searchable final
	// directory modes (for example a caller's write-only 0200).
	dirs := []*os.Root{dir}
	for i := len(created) - 1; i >= 0; i-- {
		dirs = append(dirs, created[i])
	}
	for i := len(parents) - 1; i >= 0; i-- {
		dirs = append(dirs, parents[i])
	}
	handles := map[*os.Root]*os.File{}
	defer func() {
		for _, f := range handles {
			f.Close()
		}
	}()
	for _, r := range dirs {
		if handles[r] == nil {
			f, e := r.OpenFile(".", wsfile.DirectoryFlags, 0)
			if e != nil {
				return fail(wbWriteUnknown)
			}
			handles[r] = f
		}
	}
	mode = w.mode
	if mode == 0 {
		mode = 0600
	}
	mode |= (mode & 0444) >> 2
	for i := len(created) - 1; i >= 0; i-- {
		if handles[created[i]].Chmod(workbenchFileMode(mode)) != nil {
			return fail(wbWriteUnknown)
		}
	}
	if w.fault("sync_directory") != nil {
		return fail(wbWriteUnknown)
	}
	for _, r := range dirs {
		f := handles[r]
		if f == nil {
			continue
		}
		if syncWorkbenchHandle(f) != nil {
			return fail(wbWriteUnknown)
		}
		// Duplicated roots in dirs have already been synced once.
		f.Close()
		delete(handles, r)
	}
	return ToolResult{Content: tool + ": wrote " + rel}, nil
}
