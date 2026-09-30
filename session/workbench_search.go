package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

const maxSearchFileBytes = 1 << 20
const maxSearchPatternBytes = 4096
const maxSearchMatches = 200

// searchFiles shares list_files' handle walk and never follows links.
func (w *workspace) searchFiles(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Pattern *string `json:"pattern"`
		Literal bool    `json:"literal"`
		Path    string  `json:"path"`
		Glob    string  `json:"glob"`
	}
	if json.Unmarshal(raw, &in) != nil || in.Pattern == nil || len(*in.Pattern) > maxSearchPatternBytes || len(in.Glob) > maxWorkbenchPath {
		return workbenchError(workbenchSearchFiles, wbArgumentsInvalid, ""), nil
	}
	pattern := *in.Pattern
	if in.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return workbenchError(workbenchSearchFiles, wbArgumentsInvalid, ""), nil
	}
	if _, err = path.Match(in.Glob, ""); err != nil {
		return workbenchError(workbenchSearchFiles, wbArgumentsInvalid, ""), nil
	}
	clean, code := resolveName(in.Path, true)
	if code != "" {
		return workbenchError(workbenchSearchFiles, code, in.Path), nil
	}
	if err = w.checkpoint(ctx); err != nil {
		return ToolResult{}, err
	}
	root, err := w.rootNode()
	if err != nil {
		return workbenchError(workbenchSearchFiles, wbUnreadable, clean), nil
	}
	c := w.newCursor(root, listPolicy)
	defer c.close()
	parent := root
	if clean != "." {
		parent, code = w.listTarget(c, root, path.Dir(clean))
		if code != "" {
			return workbenchError(workbenchSearchFiles, code, clean), nil
		}
	}
	skipped := map[string]int{}
	type match struct {
		rel  string
		line int
		text string
	}
	found := []match{}
	matches := []string{}
	used, visited := 0, 0
	truncated := false
	lineCuts := 0
	targetRel := clean
	if targetRel == "." {
		targetRel = ""
	}
	room := w.budget - 1024
	inspect := func(node *dirNode, name string, info fs.FileInfo) error {
		if err := w.checkpoint(ctx); err != nil {
			return err
		}
		rel := path.Join(node.rel, name)
		if info.Mode()&fs.ModeSymlink != 0 {
			skipped[wbIsSymlink]++
			return nil
		}
		if !info.Mode().IsRegular() {
			skipped[wbNotRegular]++
			return nil
		}
		if in.Glob != "" {
			target := rel
			if !strings.Contains(in.Glob, "/") {
				target = path.Base(rel)
			}
			if ok, _ := path.Match(in.Glob, target); !ok {
				return nil
			}
		}
		f, code := w.openRegular(c, root, rel)
		if code != "" {
			skipped[code]++
			return nil
		}
		defer w.release(f)
		opened, e := f.Stat()
		if e != nil {
			skipped[wbUnreadable]++
			return nil
		}
		if opened.Size() > maxSearchFileBytes {
			skipped[wbTooLarge]++
			return nil
		}
		data, large, e := wsfile.ReadAtMost(f, maxSearchFileBytes, func() error { return w.checkpoint(ctx) })
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e != nil {
			skipped[wbUnreadable]++
			return nil
		}
		if large {
			skipped[wbTooLarge]++
			return nil
		}
		if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 || !utf8.Valid(data) {
			skipped[wbNotText]++
			return nil
		}
		lines := bytes.Split(data, []byte{'\n'})
		for n, line := range lines {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !re.Match(line) {
				continue
			}
			if len(line) == 0 && n == len(lines)-1 {
				continue
			}
			full := rel + ":" + strconv.Itoa(n+1) + ": " + string(line)
			text := cutRunes(full, 400)
			if len(text) < len(full) {
				lineCuts++
			}
			if len(found) >= maxSearchMatches || used+len(text)+1 > room {
				truncated = true
				return nil
			}
			found = append(found, match{rel: rel, line: n + 1, text: text})
			used += len(text) + 1
		}
		return nil
	}
	var queue []*dirNode
	if clean == "." {
		queue = append(queue, root)
	} else {
		name := path.Base(clean)
		if hiddenName(name) {
			return workbenchError(workbenchSearchFiles, wbNotListed, clean), nil
		}
		dir, e := c.to(parent)
		if e != nil {
			return workbenchError(workbenchSearchFiles, rootFailure(e), clean), nil
		}
		info, e := dir.Lstat(name)
		if e != nil {
			return workbenchError(workbenchSearchFiles, rootFailure(e), clean), nil
		}
		if info.IsDir() {
			queue = append(queue, parent.child(name, info))
		} else if e = inspect(parent, name, info); e != nil {
			return ToolResult{}, e
		}
	}
	for len(queue) > 0 && !truncated {
		node := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if err = w.checkpoint(ctx); err != nil {
			return ToolResult{}, err
		}
		dir, e := c.to(node)
		if e != nil {
			if len(found) == 0 && node.rel == targetRel {
				return workbenchError(workbenchSearchFiles, rootFailure(e), clean), nil
			}
			if errors.Is(e, errOtherMount) {
				skipped[wbOtherMount]++
			} else if !errors.Is(e, errHidden) {
				skipped["directories_incomplete"]++
			}
			continue
		}
		seen := 0
		e = w.eachName(ctx, dir, node.rel, func(name string) (bool, error) {
			if visited >= w.visitLimit() {
				truncated = true
				return false, nil
			}
			visited++
			seen++
			if hiddenName(name) {
				return true, ctx.Err()
			}
			info, e := dir.Lstat(name)
			if e != nil {
				skipped[wbUnreadable]++
				return true, ctx.Err()
			}
			if info.IsDir() {
				queue = append(queue, node.child(name, info))
				return true, ctx.Err()
			}
			e = inspect(node, name, info)
			// inspect may move the cursor. Restore the directory for the next name.
			if e == nil {
				dir, e = c.to(node)
			}
			return !truncated, e
		})
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		if e != nil {
			if node.rel == targetRel && seen == 0 {
				return workbenchError(workbenchSearchFiles, rootFailure(e), clean), nil
			}
			if errors.Is(e, errOtherMount) {
				skipped[wbOtherMount]++
			} else if !errors.Is(e, errHidden) {
				skipped["directories_incomplete"]++
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].rel != found[j].rel {
			return found[i].rel < found[j].rel
		}
		return found[i].line < found[j].line
	})
	for _, m := range found {
		matches = append(matches, m.text)
	}
	keys := make([]string, 0, len(skipped))
	for key := range skipped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	notes := []string{}
	for _, key := range keys {
		notes = append(notes, key+"="+strconv.Itoa(skipped[key]))
	}
	if lineCuts > 0 {
		notes = append(notes, "lines_cut="+strconv.Itoa(lineCuts))
	}
	if truncated {
		notes = append(notes, "truncated; narrow path or glob")
	}
	if len(notes) > 0 {
		matches = append(matches, "[search_files: "+strings.Join(notes, ", ")+"]")
	}
	if len(matches) == 0 {
		return ToolResult{Content: "[search_files: no matches]"}, nil
	}
	return ToolResult{Content: strings.Join(matches, "\n")}, nil
}
