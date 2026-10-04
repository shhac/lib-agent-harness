package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness/internal/textbound"
	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// readFile answers read_file: a regular UTF-8 file of at most 16 MiB, from
// line offset, at most limit lines and the result budget.
func (w *Workspace) readFile(ctx context.Context, raw json.RawMessage) (Result, error) {
	if ToolAvailability(workbenchReadFile) != "" {
		return contentRefusal(workbenchReadFile)
	}
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
		return Result{}, err
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
		return Result{}, ctx.Err()
	case err != nil:
		return workbenchError(workbenchReadFile, wbUnreadable, clean), nil
	case tooLarge:
		return workbenchError(workbenchReadFile, wbTooLarge, clean), nil
	case !wsfile.Text(data):
		return workbenchError(workbenchReadFile, wbNotText, clean), nil
	}
	return Result{Content: readLines(data, offset, limit, w.budget)}, nil
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
				shown := textbound.Cut(string(data[start:min(next, start+room+utf8.UTFMax)]), room)
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
