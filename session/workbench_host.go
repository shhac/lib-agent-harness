package session

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/shhac/lib-agent-harness/sandbox"
)

type workbenchHost struct {
	stuck    atomic.Pointer[TurnError]
	files    *sandbox.Workspace
	commands *sandbox.Runner
	budget   int
	id       string
	failed   func(error)
	options  Options
}

// openWorkspaceFiles is replaced by session tests to inject workspace faults.
var openWorkspaceFiles = sandbox.OpenWorkspace

func openWorkspace(o Options, id string) (*workbenchHost, error) {
	if o.Workbench == nil {
		return nil, nil
	}
	budget := sandbox.MaxResult
	if limit := o.Restriction.Tools.MaxResultBytes; limit > 0 && limit < budget {
		budget = limit
	}
	w := &workbenchHost{budget: budget, options: o, id: id}
	files, err := openWorkspaceFiles(sandbox.Config{Root: o.WorkDir, NewFileMode: o.Workbench.NewFileMode, Budget: budget, SessionID: id, OnFailure: func(err error) {
		failure := w.fromSandbox(err)
		if w.failed != nil {
			w.failed(failure)
		}
	}})
	if err != nil {
		return nil, fromSandbox(err, o)
	}
	w.files = files
	return w, nil
}
func (w *workbenchHost) close() {
	if w != nil {
		w.files.Close()
	}
}
func (w *workbenchHost) setFailed(f func(error)) { w.failed = f }

// fromSandbox retains the one stuck failure observed by the turn, health and
// Release, including its identity after an earlier session failure.
func (w *workbenchHost) fromSandbox(err error) error {
	translated := fromSandbox(err, w.options)
	var failure *TurnError
	if errors.As(translated, &failure) && failure.Code == WorkspaceIOStuck {
		w.stuck.CompareAndSwap(nil, failure)
		return w.stuck.Load()
	}
	return translated
}
func (w *workbenchHost) stuckError() error {
	if failure := w.stuck.Load(); failure != nil {
		return failure
	}
	if w.files != nil && w.files.Stuck() {
		return w.fromSandbox(&sandbox.CommandError{Code: WorkspaceIOStuck})
	}
	return nil
}

func resolveName(name string, dir bool) (string, string) { return sandbox.ResolveName(name, dir) }
func workbenchError(tool, code, rel string) ToolResult {
	r := sandbox.ToolError(tool, code, rel)
	return ToolResult{Content: r.Content, IsError: r.IsError}
}

const (
	maxWorkbenchResult = sandbox.MaxResult
	minWorkbenchResult = sandbox.MinResult
	wbArgumentsInvalid = sandbox.ArgumentsInvalid
	wbWriteFailed      = sandbox.WriteFailed
	wbWriteUnknown     = sandbox.WriteUnknown
)

var errWorkbenchWriteUnknown = sandbox.ErrWriteUnknown

func (w *workbenchHost) checkDir(ctx context.Context, rel string) string {
	return w.files.CheckDir(ctx, rel)
}
