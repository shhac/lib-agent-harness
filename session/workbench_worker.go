package session

import (
	"errors"

	"github.com/shhac/lib-agent-harness/sandbox"
)

// WorkspaceIOStuck reports a cancelled workspace syscall that did not settle.
const WorkspaceIOStuck = sandbox.WorkspaceIOStuck

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
