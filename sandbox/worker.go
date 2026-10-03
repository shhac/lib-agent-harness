package sandbox

import (
	"context"
	"time"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// WorkspaceIOStuck reports a cancelled workspace syscall that did not settle.
const WorkspaceIOStuck = "workspace_io_stuck"

var workspaceGrace = 10 * time.Second
var workspaceMountID = wsfile.MountID

type workspaceAnswer struct {
	result Result
	err    error
}
type workspaceJob struct {
	run    func() (Result, error)
	answer chan workspaceAnswer
}

// worker is the only goroutine doing tool workspace I/O. A failed session
// abandons at most this one worker; it exits after its current syscall returns.
func (w *Workspace) worker() {
	defer close(w.stopped)
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		select {
		case <-w.stop:
			return
		case job := <-w.jobs:
			r, err := job.run()
			job.answer <- workspaceAnswer{r, err}
		}
	}
}

func (w *Workspace) dispatch(ctx context.Context, run func() (Result, error)) (Result, error) {
	return w.dispatchResult(ctx, run, false)
}

// A write's settled answer outranks cancellation: a committed rename must
// finish durability and a pre-commit cancellation is a definite failure.
func (w *Workspace) dispatchWrite(ctx context.Context, run func() (Result, error)) (Result, error) {
	return w.dispatchResult(ctx, run, true)
}

func (w *Workspace) dispatchResult(ctx context.Context, run func() (Result, error), keepAnswer bool) (Result, error) {
	job := workspaceJob{run: run, answer: make(chan workspaceAnswer, 1)}
	select {
	case <-w.stop:
		return Result{}, ErrClosed
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case w.jobs <- job:
	}
	select {
	case answer := <-job.answer:
		return answer.result, answer.err
	case <-ctx.Done():
	}
	timer := time.NewTimer(w.grace)
	defer timer.Stop()
	select {
	case answer := <-job.answer:
		if !keepAnswer {
			return Result{}, ctx.Err()
		}
		return answer.result, answer.err
	case <-timer.C:
		err := &CommandError{Code: WorkspaceIOStuck}
		w.stuck.Store(err)
		if w.failed != nil {
			w.failed(err)
		}
		w.close()
		return Result{}, err
	}
}
