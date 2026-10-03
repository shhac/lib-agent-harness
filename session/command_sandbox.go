package session

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// CommandSandboxOptions selects the same proved boundary as Workbench.Commands.
// RuntimeHome must be an existing private directory outside WorkDir.
type CommandSandboxOptions struct {
	WorkDir, RuntimeHome string
	Write                bool
	Read, Env            []string
	Loopback             bool
	Timeout              time.Duration
	Background           bool
}

// CommandRequest runs a shell command in a relative workspace directory.
// Timeout defaults to the sandbox's bound and may only shorten it for Run.
// Start has no timeout and refuses a nonzero Timeout.
type CommandRequest struct {
	Command, Dir string
	Timeout      time.Duration
}

// CommandResult distinguishes an observed exit from a timeout.
type CommandResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
}

// Stable typed command-failure codes, carried by TurnError.
const (
	CommandStartFailed    = "command_start_failed"
	CommandOutcomeUnknown = "command_outcome_unknown"
	CommandProcessLimit   = "command_process_limit"
	CommandCleanupUnknown = "command_cleanup_unknown"
	CommandSandboxClosed  = "command_sandbox_closed"
)

// CommandSandbox owns commands without opening a model session. Close stops
// admission, cancels admitted work, waits for settlement and reaps descendants.
type CommandSandbox struct {
	mu       sync.Mutex
	ws       *workspace
	loopback bool
	closing  bool
	active   map[*StartedCommand]struct{}
	wg       sync.WaitGroup
	once     sync.Once
	closeErr error
	dir      string
	lock     *os.File
}

// OpenCommandSandbox proves the installed sandbox before creating command state.
func OpenCommandSandbox(ctx context.Context, opts CommandSandboxOptions) (*CommandSandbox, error) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, WorkDir: opts.WorkDir, RuntimeHome: opts.RuntimeHome, Background: opts.Background,
		Restriction: &Restriction{Tools: ToolHost{}},
		Workbench:   &Workbench{standaloneCommands: true, Write: opts.Write, Commands: &Commands{Loopback: opts.Loopback, Read: opts.Read, Env: opts.Env, Timeout: opts.Timeout}}}
	// Match the workbench's pre-launch refusal even without a runtime home.
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		_, err := normalizeWorkbench(o)
		return nil, err
	}
	var err error
	if o, err = normalizeRuntimeHome(o); err != nil {
		return nil, err
	}
	if o, err = normalizeWorkbench(o); err != nil {
		return nil, err
	}
	if err = proveWorkbench(ctx, o); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	w, err := openWorkspace(o)
	if err != nil {
		return nil, err
	}
	s := &CommandSandbox{ws: w, loopback: opts.Loopback, active: make(map[*StartedCommand]struct{})}
	if err = prepareCommandState(ctx, s, &o); err != nil {
		w.close()
		return nil, err
	}
	if w.commands, err = newCommandSandbox(commandConfig{options: o, outputBudget: w.budget}); err != nil {
		w.close()
		s.lock.Close() // preserve any recovery marker
		return nil, err
	}
	return s, nil
}

func commandError(code string) error { return &TurnError{Engine: harness.OpenAICompatible, Code: code} }

// Run executes a bounded command. Cancellation settles its tree before returning.
func (s *CommandSandbox) Run(ctx context.Context, req CommandRequest) (CommandResult, error) {
	h, err := s.admit(ctx, req, false)
	if err != nil {
		return CommandResult{}, err
	}
	return h.Result()
}

// Start starts a command until it exits, its context is cancelled, Stop or Close.
// It reports process launch, not server readiness. Linux refuses Loopback here:
// the private namespace cannot expose a server to the owner's browser.
func (s *CommandSandbox) Start(ctx context.Context, req CommandRequest) (*StartedCommand, error) {
	s.mu.Lock()
	closed := s.closing
	s.mu.Unlock()
	if closed {
		return nil, commandError(CommandSandboxClosed)
	}
	if runtime.GOOS == "linux" && s.loopback {
		return nil, &UnsupportedError{Engine: harness.OpenAICompatible, Operation: "start", Code: RefusedNotOffered,
			Capability: harness.Capability{Availability: harness.Unsupported, Reason: "bubblewrap's private localhost is unreachable from the host; use Run for a server and client in one command"}}
	}
	h, err := s.admit(ctx, req, true)
	if err != nil {
		return nil, err
	}
	select {
	case <-h.launched:
	case <-h.done:
		if h.err != nil {
			return nil, h.err
		}
	case <-ctx.Done():
		h.Stop()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		h.Stop()
		return nil, err
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		h.Stop()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, commandError(CommandSandboxClosed)
	}
	return h, nil
}

func (s *CommandSandbox) admit(ctx context.Context, req CommandRequest, start bool) (*StartedCommand, error) {
	s.mu.Lock()
	closed := s.closing
	s.mu.Unlock()
	if closed {
		return nil, commandError(CommandSandboxClosed)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Command == "" || len(req.Command) > 64<<10 || strings.ContainsRune(req.Command, 0) || req.Timeout < 0 || req.Timeout > s.ws.commands.timeout || (start && req.Timeout != 0) {
		return nil, commandError(wbArgumentsInvalid)
	}
	rel, code := resolveName(req.Dir, true)
	if code != "" {
		return nil, commandError(code)
	}
	c, _, _, code := s.ws.writeParent(ctx, rel+"/placeholder", false)
	if c != nil {
		c.close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, commandError(CommandSandboxClosed)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if code != "" {
		return nil, commandError(code)
	}
	if len(s.active) >= 64 {
		return nil, commandError(CommandProcessLimit)
	}
	runCtx, cancel := context.WithCancel(ctx)
	h := &StartedCommand{cancel: cancel, done: make(chan struct{}), launched: make(chan struct{})}
	s.active[h] = struct{}{}
	s.wg.Add(1)
	timeout := req.Timeout
	if timeout == 0 {
		timeout = s.ws.commands.timeout
	}
	if start {
		timeout = 0
	}
	go func() {
		defer s.wg.Done()
		defer cancel()
		h.result, h.err = s.ws.commands.execute(runCtx, req.Command, rel, timeout, func() { close(h.launched) })
		s.mu.Lock()
		if h.err != nil && runCtx.Err() != nil {
			h.err = runCtx.Err()
			if ctx.Err() != nil {
				h.err = ctx.Err()
			} else if s.closing {
				h.err = commandError(CommandSandboxClosed)
			}
		}
		close(h.done)
		delete(s.active, h)
		s.mu.Unlock()
	}()
	return h, nil
}

// Close is idempotent, including its error. Failed cleanup retains recovery state.
func (s *CommandSandbox) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closing = true
		for h := range s.active {
			h.cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
		s.closeErr = s.ws.commands.close()
		s.ws.close()
		if s.closeErr == nil && os.RemoveAll(s.dir) != nil {
			s.closeErr = commandError(CommandCleanupUnknown)
		}
		if s.lock != nil {
			_ = s.lock.Close()
		}
	})
	return s.closeErr
}

// StartedCommand holds bounded output until settlement. Result waits for Done.
type StartedCommand struct {
	cancel         context.CancelFunc
	done, launched chan struct{}
	result         CommandResult
	err            error
}

// Stop cancels and waits for the command tree. Repeated calls are harmless.
func (h *StartedCommand) Stop() { h.cancel(); <-h.done }

// Done closes once the command tree and output have settled.
func (h *StartedCommand) Done() <-chan struct{} { return h.done }

// Result waits for Done and returns the observed outcome. An error means the
// result must not be treated as a successful or known exit observation.
func (h *StartedCommand) Result() (CommandResult, error) { <-h.done; return h.result, h.err }

func notifyCommandLaunch(onStart func()) {
	if onStart != nil {
		onStart()
	}
}
func commandContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}
