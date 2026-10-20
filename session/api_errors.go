package session

import (
	harness "github.com/shhac/lib-agent-harness"
)

// State failure codes: an OpenAI-compatible session's own durable state.
const (
	// StateLocked: another session, in this process or another, has the
	// conversation open. It unwraps to ErrLeaseHeld.
	StateLocked = "session_locked"
	// StateUnusable: RuntimeHome or the session's directory cannot be used.
	StateUnusable = "state_unusable"
	// StateMissing: the reference's conversation is not in RuntimeHome. Open
	// starts a new one with FreshUnavailable.
	StateMissing = "conversation_missing"
	// StateCorrupt: the transcript cannot be read, or names another session.
	StateCorrupt = "transcript_unreadable"
	// StateUnwritable: a record could not be made durable. The session ends,
	// because its history could no longer be trusted to survive a restart.
	StateUnwritable = "transcript_unwritable"
)

// StateError reports that a session's durable state could not be used.
type StateError struct {
	Engine harness.Engine
	Code   string
}

func (e *StateError) Error() string {
	message := map[string]string{
		StateLocked:     "the conversation is open in another session",
		StateUnusable:   "the session's runtime home cannot be used",
		StateMissing:    "the conversation is not in the runtime home",
		StateCorrupt:    "the session's transcript cannot be read",
		StateUnwritable: "the session's transcript could not be written",
	}[e.Code]
	if message == "" {
		message = "the session's state cannot be used"
	}
	return string(e.Engine) + ": " + message
}

func (e *StateError) Unwrap() error {
	switch e.Code {
	case StateLocked:
		return ErrLeaseHeld
	case StateMissing:
		return errConversationGone
	}
	return nil
}

func (e *StateError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: e.Engine, Operation: harness.Session, Family: harness.FailurePreflight, Code: e.Code}
}

func stateError(code string) *StateError {
	return &StateError{Engine: harness.OpenAICompatible, Code: code}
}
