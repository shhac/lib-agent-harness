package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"time"
)

// Config carries effective workspace settings, after the caller's policy
// validation. Session supplies its normalized permissions and result budget.
type Config struct {
	// Root selects an existing workspace directory.
	Root string
	// NewFileMode is the effective mode for new files, including zero.
	NewFileMode fs.FileMode
	// Budget is capped at MaxResult; zero uses MaxResult. Callers should use
	// at least MinResult so bounded results have room for their closing notes.
	Budget int
	// SessionID selects the reserved-temporary namespace, using the caller's
	// stable session identifier. It must be set before writing.
	SessionID string
	// Grace bounds the wait for cancelled I/O to settle; zero uses ten seconds.
	Grace time.Duration
	// OnFailure receives a late I/O settlement failure, including one after Close.
	// It runs on the dispatching goroutine and must not reenter workspace tools.
	OnFailure func(error)
}

// Result is the bounded, model-facing text of one file operation.
type Result struct {
	Content string
	IsError bool
}

const (
	workbenchReadFile    = "read_file"
	workbenchListFiles   = "list_files"
	workbenchSearchFiles = "search_files"
	workbenchWriteFile   = "write_file"
	workbenchEditFile    = "edit_file"
	// MaxResult is the maximum byte budget for a file result.
	MaxResult = maxWorkbenchResult
	// MinResult leaves room for a bounded file result and its closing notes.
	MinResult = minWorkbenchResult
	// ArgumentsInvalid is the file-tool result code for malformed arguments.
	ArgumentsInvalid = wbArgumentsInvalid
	// WriteFailed is the file-tool result code for a definite write failure.
	WriteFailed = wbWriteFailed
	// WriteUnknown is the file-tool result code for an unsettled write outcome.
	WriteUnknown = wbWriteUnknown
)

// ErrWriteUnknown identifies a write whose replacement happened but durability
// could not be established; callers must not infer rollback.
var ErrWriteUnknown = errWorkbenchWriteUnknown

// Close closes admission and releases the root. It is nil-safe and idempotent;
// it does not wait for an abandoned syscall to return.
func (w *Workspace) Close() { w.close() }

// Stuck reports whether cancelled I/O exceeded its settlement grace.
func (w *Workspace) Stuck() bool { return w.stuck.Load() != nil }

// Read executes read_file through the workspace worker. Linux and macOS
// refuse it as not_offered before content I/O pending verified admission.
func (w *Workspace) Read(ctx context.Context, raw json.RawMessage) (Result, error) {
	return w.dispatch(ctx, func() (Result, error) { return w.readFile(ctx, raw) })
}

// List executes list_files through the workspace worker.
func (w *Workspace) List(ctx context.Context, raw json.RawMessage) (Result, error) {
	return w.dispatch(ctx, func() (Result, error) { return w.listFiles(ctx, raw) })
}

// Search executes search_files through the workspace worker. Linux and macOS
// refuse it as not_offered before content I/O pending verified admission.
func (w *Workspace) Search(ctx context.Context, raw json.RawMessage) (Result, error) {
	return w.dispatch(ctx, func() (Result, error) { return w.searchFiles(ctx, raw) })
}

// Write executes write_file, retaining the settled commit result on cancellation.
func (w *Workspace) Write(ctx context.Context, raw json.RawMessage) (Result, error) {
	return w.write(ctx, workbenchWriteFile, raw)
}

// Edit executes edit_file, retaining the settled commit result on cancellation.
// Linux and macOS refuse it as not_offered before content I/O.
func (w *Workspace) Edit(ctx context.Context, raw json.RawMessage) (Result, error) {
	return w.write(ctx, workbenchEditFile, raw)
}
func (w *Workspace) write(ctx context.Context, tool string, raw json.RawMessage) (Result, error) {
	r, err := w.dispatchWrite(ctx, func() (Result, error) { return w.writeFile(ctx, tool, raw) })
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return workbenchError(tool, wbWriteFailed, ""), nil
	}
	return r, err
}

// ToolError formats a bounded file-tool refusal without host paths or OS prose.
func ToolError(tool, code, rel string) Result { return workbenchError(tool, code, rel) }

// ResolveName checks a slash-relative path; directory allows the root itself.
func ResolveName(name string, directory bool) (string, string) { return resolveName(name, directory) }

// CheckDir checks a directory returned by ResolveName with a symlink-free walk.
// It returns a fixed result code, or an empty string for success. Commands must
// still enforce their OS boundary if a pathname changes after this check.
func (w *Workspace) CheckDir(ctx context.Context, rel string) string {
	c, _, _, code := w.writeParent(ctx, rel+"/placeholder", false)
	if c != nil {
		c.close()
	}
	return code
}
