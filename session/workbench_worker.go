package session

import (
	"context"
	"errors"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// WorkspaceIOStuck reports a cancelled workspace syscall that did not settle.
const WorkspaceIOStuck = "workspace_io_stuck"

var workspaceGrace = 10 * time.Second
var workspaceMountID = wsfile.MountID

type workspaceAnswer struct {
	result ToolResult
	err    error
}
type workspaceJob struct {
	run    func() (ToolResult, error)
	answer chan workspaceAnswer
}

// worker is the only goroutine doing tool workspace I/O. A failed session
// abandons at most this one worker; it exits after its current syscall returns.
func (w *workspace) worker() {
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

func (w *workspace) dispatch(ctx context.Context, run func() (ToolResult, error)) (ToolResult, error) {
	job := workspaceJob{run: run, answer: make(chan workspaceAnswer, 1)}
	select {
	case <-w.stop:
		return ToolResult{}, ErrClosed
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
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
	case <-job.answer:
		return ToolResult{}, ctx.Err()
	case <-timer.C:
		err := &TurnError{Engine: harness.OpenAICompatible, Code: WorkspaceIOStuck}
		w.stuck.Store(err)
		if w.failed != nil {
			w.failed(err)
		}
		w.close()
		return ToolResult{}, err
	}
}

func workspaceStuck(err error) bool {
	var failure *TurnError
	return errors.As(err, &failure) && failure.Code == WorkspaceIOStuck
}

// failWorkspace retains a late I/O failure even when Close already marked the
// session closed. Release must report the worker that shutdown abandoned.
func (s *Session) failWorkspace(err error) {
	s.mu.Lock()
	if s.failure == nil || errors.Is(s.failure, ErrClosed) {
		s.failure = err
	}
	s.mu.Unlock()
	s.fail(err)
}
