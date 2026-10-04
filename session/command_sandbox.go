package session

import (
	"context"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
)

// CommandSandboxOptions selects a proved standalone command boundary.
// RuntimeHome must be an existing private directory outside WorkDir.
//
// Deprecated: use sandbox.Options.
type CommandSandboxOptions struct {
	WorkDir, RuntimeHome string
	Write                bool
	Read, Env            []string
	Loopback             bool
	// LoopbackPorts is nil for existing loopback behavior; otherwise 1–32 ports.
	// Selected ports require Loopback and a proved macOS Seatbelt boundary.
	LoopbackPorts []int
	// LoopbackControl optionally pins the off-machine DNS control IP literal.
	LoopbackControl string
	Timeout         time.Duration
	Background      bool
}

// CommandRequest selects a shell command.
//
// Deprecated: use sandbox.CommandRequest.
type CommandRequest = sandbox.CommandRequest

// CommandResult reports an observed command outcome.
//
// Deprecated: use sandbox.CommandResult.
type CommandResult = sandbox.CommandResult

const (
	// CommandStartFailed is the retained standalone command code.
	//
	// Deprecated: use sandbox.CommandStartFailed.
	CommandStartFailed = sandbox.CommandStartFailed
	// CommandOutcomeUnknown is the retained standalone command code.
	//
	// Deprecated: use sandbox.CommandOutcomeUnknown.
	CommandOutcomeUnknown = sandbox.CommandOutcomeUnknown
	// CommandProcessLimit is the retained standalone command code.
	//
	// Deprecated: use sandbox.CommandProcessLimit.
	CommandProcessLimit = sandbox.CommandProcessLimit
	// CommandCleanupUnknown is the retained standalone command code.
	//
	// Deprecated: use sandbox.CommandCleanupUnknown.
	CommandCleanupUnknown = sandbox.CommandCleanupUnknown
	// CommandSandboxClosed is the retained standalone command code.
	//
	// Deprecated: use sandbox.CommandSandboxClosed.
	CommandSandboxClosed = sandbox.CommandSandboxClosed
)

// CommandSandbox owns commands without a model session.
//
// Deprecated: use sandbox.Sandbox.
type CommandSandbox struct {
	inner    *sandbox.Sandbox
	once     sync.Once
	closeErr error
}

var commandSandboxTranslation = Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}}

// OpenCommandSandbox proves and opens a standalone command boundary.
//
// Deprecated: use sandbox.Open.
func OpenCommandSandbox(ctx context.Context, o CommandSandboxOptions) (*CommandSandbox, error) {
	s, err := sandbox.Open(ctx, sandbox.Options{WorkDir: o.WorkDir, RuntimeHome: o.RuntimeHome, Write: o.Write, Read: o.Read, Env: o.Env, Loopback: o.Loopback, LoopbackPorts: o.LoopbackPorts, LoopbackControl: o.LoopbackControl, Timeout: o.Timeout, Background: o.Background})
	if err != nil {
		return nil, fromSandbox(err, commandSandboxTranslation)
	}
	return &CommandSandbox{inner: s}, nil
}

// Run runs a command until settlement.
//
// Deprecated: use sandbox.Sandbox.Run.
func (s *CommandSandbox) Run(ctx context.Context, r CommandRequest) (CommandResult, error) {
	result, err := s.inner.Run(ctx, r)
	return result, fromSandbox(err, commandSandboxTranslation)
}

// Start returns after command launch.
// A failure after launch returns a settled non-nil handle with diagnostics.
// A pre-launch failure returns nil.
//
// Deprecated: use sandbox.Sandbox.Start.
func (s *CommandSandbox) Start(ctx context.Context, r CommandRequest) (*StartedCommand, error) {
	h, err := s.inner.Start(ctx, r)
	if h == nil {
		return nil, fromSandbox(err, commandSandboxTranslation)
	}
	return &StartedCommand{inner: h}, fromSandbox(err, commandSandboxTranslation)
}

// Close settles commands and releases sandbox state.
//
// Deprecated: use sandbox.Sandbox.Close.
func (s *CommandSandbox) Close() error {
	s.once.Do(func() { s.closeErr = fromSandbox(s.inner.Close(), commandSandboxTranslation) })
	return s.closeErr
}

// StartedCommand holds output until command settlement.
//
// Deprecated: use sandbox.StartedCommand.
type StartedCommand struct {
	inner  *sandbox.StartedCommand
	once   sync.Once
	result CommandResult
	err    error
}

// Stop requests command cancellation.
//
// Deprecated: use sandbox.StartedCommand.Stop.
func (h *StartedCommand) Stop() { h.inner.Stop() }

// Done closes when the command settles.
//
// Deprecated: use sandbox.StartedCommand.Done.
func (h *StartedCommand) Done() <-chan struct{} { return h.inner.Done() }

// Result waits for and returns the settled outcome.
//
// Deprecated: use sandbox.StartedCommand.Result.
func (h *StartedCommand) Result() (CommandResult, error) {
	h.once.Do(func() { r, err := h.inner.Result(); h.result, h.err = r, fromSandbox(err, commandSandboxTranslation) })
	return h.result, h.err
}
