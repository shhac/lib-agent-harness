package session

import (
	"errors"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
)

// fromSandbox preserves session's error vocabulary at the package boundary.
func fromSandbox(err error, o Options) error {
	if err == nil {
		return nil
	}
	var refusal *sandbox.RefusalError
	if errors.As(err, &refusal) {
		return &UnsupportedError{Engine: o.Provider.Engine, Operation: refusal.Operation, Code: refusal.Code, Capability: refusal.Capability}
	}
	var proof *sandbox.ProofError
	if errors.As(err, &proof) {
		return &CapabilityError{Engine: harness.OpenAICompatible, Code: proof.Code, Phase: BeforeLaunch, Tools: proof.Tools}
	}
	var command *sandbox.CommandError
	if errors.As(err, &command) {
		return &TurnError{Engine: harness.OpenAICompatible, Code: command.Code}
	}
	if errors.Is(err, sandbox.ErrClosed) {
		return ErrClosed
	}
	return err
}
