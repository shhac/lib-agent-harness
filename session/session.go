package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type Session struct {
	opGate    chan struct{} // serializes controls without blocking cancelled callers
	eventMu   sync.Mutex
	mu        sync.Mutex
	options   Options
	ref       Ref
	caps      Capabilities
	telemetry Telemetry
	transport wire
	active    *Turn
	closed    bool
	failure   error
	lastEvent time.Time
	tools     *toolHost
	// identified carries the outcome of durably recording the running harness's
	// identity. A restricted session waits for it before its first inference.
	identified chan error
	done       chan struct{}
}

// Start opens a persistent native CLI. ctx owns its lifetime; cancelling it
// terminates the process tree. Startup performs a handshake, not inference.
func Start(ctx context.Context, o Options) (*Session, error) { return open(ctx, o, nil) }

// Resume reopens exactly the configuration described by a saved reference. An
// account label cannot detect a user logging a different account into the same
// home; the caller must update AccountIdentity when doing so.
func Resume(ctx context.Context, o Options, r Ref) (*Session, error) { return open(ctx, o, &r) }
func open(ctx context.Context, o Options, r *Ref) (*Session, error) {
	o, err := normalize(o)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if r != nil && !compatible(o, *r) {
		return nil, ErrIncompatibleResume
	}
	// The restricted runtime is prepared and proved first. Nothing below this
	// point runs with the caller's login until the harness has demonstrated,
	// against a provider that refuses to infer, that it dropped its own tools.
	l, err := prepareLaunch(ctx, o)
	if err != nil {
		return nil, err
	}
	s := &Session{options: o, ref: reference(o, ""), caps: CapabilitiesFor(o.Engine), done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	if l != nil {
		s.tools = l.host
		l.host.onRefusal = s.toolRefused
		l.host.activeTurn = func() string {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.active == nil {
				return ""
			}
			return s.active.ID()
		}
	}
	if r != nil {
		s.ref = *r
	} else if o.Engine == Claude {
		s.ref.ID = newID()
	}
	// A restricted session marks the attempt before the harness exists, then
	// names the process once it is running and contained. Doing only the second
	// would leave a window where a harness is alive and nothing on disk says so.
	// Doing only the first would leave nothing to signal. Both, in that order,
	// mean a crash at any point is either "no process" or "a process, reserved".
	var onStart func(int)
	if l != nil {
		if err = recordLaunch(l.host.cfg.Dir, launchRecord{Engine: string(o.Engine), Launch: l.host.socketDir, Started: time.Now().UTC()}); err != nil {
			s.releaseTools()
			return nil, err
		}
		identified := make(chan error, 1)
		s.identified = identified
		onStart = func(pid int) {
			record := launchRecord{Engine: string(o.Engine), PID: pid, Group: pid, Launch: l.host.socketDir, Started: time.Now().UTC()}
			err := recordLaunch(l.host.cfg.Dir, record)
			identified <- err
			if err != nil {
				s.fail(err)
			}
		}
	}
	// Reader callbacks may fire before startup returns; the transport assignment
	// is protected so an early process failure cannot race Close.
	s.mu.Lock()
	w, err := newProcessWire(ctx, o, s.ref.ID, r != nil, l, onStart, s.notification, s.fail)
	s.transport = w
	s.mu.Unlock()
	if err != nil {
		s.settleFailedLaunch(l)
		s.releaseTools()
		return nil, err
	}
	// Nothing is asked of this session until its identity is durably recorded.
	// A harness whose identity could not be written is one a later process could
	// not find, so it is stopped here rather than allowed to start work.
	if err = s.awaitIdentity(ctx); err != nil {
		s.fail(err)
		s.settleFailedLaunch(l)
		return nil, err
	}
	if err = s.initialize(ctx, r != nil); err != nil {
		s.fail(err)
		s.settleFailedLaunch(l)
		return nil, err
	}
	return s, nil
}

// settleFailedLaunch clears the marker when this launch demonstrably produced
// nothing to reclaim.
//
// The marker exists so a crash between "about to spawn" and "spawned" is held
// rather than assumed away. But a launch that failed in front of us is not that
// case: a missing binary, a harness that exited during its handshake, an
// identity that could not be written — each is a known outcome, and leaving a
// marker naming no process behind would turn one repairable failure into an
// assignment that no later resume could ever unblock. So the marker is settled
// when the process is confirmed gone, and kept when it is not.
func (s *Session) settleFailedLaunch(l *launch) {
	if l == nil {
		return
	}
	s.mu.Lock()
	w, _ := s.transport.(*streamWire)
	s.mu.Unlock()
	if w != nil {
		w.close()
		select {
		case <-w.reaped:
		case <-time.After(5 * time.Second):
			// Still there. That is exactly the uncertainty the marker is for.
			return
		}
	}
	record, err := readLaunchRecord(l.host.cfg.Dir)
	if err != nil {
		return
	}
	if record != nil && record.identified() {
		// A process existed. Whether it is gone is Reclaim's question, not this
		// one, and answering it here would risk clearing a live harness's marker.
		alive, aliveErr := groupAlive(record.Group)
		if aliveErr != nil || alive {
			return
		}
	}
	_ = clearLaunchRecord(l.host.cfg.Dir)
}

// awaitIdentity waits for the launch record to name the running harness. It is
// bounded by the caller's context and by the transport failing, so a harness
// that never starts does not hang the open.
func (s *Session) awaitIdentity(ctx context.Context) error {
	s.mu.Lock()
	identified := s.identified
	s.mu.Unlock()
	if identified == nil {
		return nil
	}
	select {
	case err := <-identified:
		return err
	case <-s.done:
		return s.transportFailure()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitReaped waits for the transport's process to be collected, bounded by the
// caller's context. It is best effort: a harness that will not die is exactly
// what the reclamation that follows is for.
func (s *Session) awaitReaped(ctx context.Context) {
	s.mu.Lock()
	w, _ := s.transport.(*streamWire)
	s.mu.Unlock()
	if w == nil {
		return
	}
	select {
	case <-w.reaped:
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
}

func (s *Session) transportFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	return ErrClosed
}

// releaseTools shuts the private tool channel. It is safe before a session has
// one, and idempotent afterwards.
func (s *Session) releaseTools() {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host != nil {
		host.close()
	}
}

// ToolsClosed reports that a closing tool has ended this session's tool
// channel. It is the only signal that means the work asked to stop; a status,
// a quiet period or a finished turn is not one.
func (s *Session) ToolsClosed() bool {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	return host != nil && host.channelClosed()
}

func (s *Session) toolRefused(tool, reason string) {
	s.mu.Lock()
	t := s.active
	s.mu.Unlock()
	if t == nil {
		return
	}
	s.emit(t, Event{Kind: "tool_refused", Tool: tool, Status: reason})
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cryptographic random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func (s *Session) initialize(ctx context.Context, resume bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.options.Engine == Codex {
		if _, err := s.transport.request(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "lib-agent-harness", "version": "1"}, "capabilities": map[string]any{}}); err != nil {
			return err
		}
		if err := s.transport.send(ctx, map[string]any{"method": "initialized"}); err != nil {
			return err
		}
		p := map[string]any{"cwd": s.options.WorkDir, "approvalPolicy": s.options.Policy.CodexApproval, "sandbox": s.options.Policy.CodexSandbox}
		if s.options.Model != "" {
			p["model"] = s.options.Model
		}
		if s.options.Instructions.Mode == Replace {
			p["baseInstructions"] = s.options.Instructions.Text
		} else if s.options.Instructions.Mode == Append {
			p["developerInstructions"] = s.options.Instructions.Text
		}
		method := "thread/start"
		if resume {
			method = "thread/resume"
			p["threadId"] = s.ref.ID
		}
		body, err := s.transport.request(ctx, method, p)
		if err != nil {
			return err
		}
		var response struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(body, &response) != nil || response.Thread.ID == "" {
			return ErrProtocol
		}
		if resume && response.Thread.ID != s.ref.ID {
			return ErrProtocol
		}
		s.mu.Lock()
		s.ref.ID = response.Thread.ID
		s.mu.Unlock()
	} else {
		body, err := s.transport.request(ctx, "initialize", map[string]any{})
		if err != nil {
			return err
		}
		s.observeClaudeAccount(body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	c := Capability{Native, "acknowledged by the installed harness"}
	if resume {
		s.caps.Resume = c
	} else {
		s.caps.Start = c
	}
	// Instructions are sent through explicit native parameters. CLI startup or
	// thread creation rejects flags/fields it cannot parse; no user-role emulation.
	if s.options.Instructions.Mode == Replace {
		s.caps.ReplaceInstructions = c
	} else if s.options.Instructions.Mode == Append {
		s.caps.AppendInstructions = c
	}
	return nil
}
func (s *Session) Ref() Ref                   { s.mu.Lock(); defer s.mu.Unlock(); return s.ref }
func (s *Session) Capabilities() Capabilities { s.mu.Lock(); defer s.mu.Unlock(); return s.caps }

// Close terminates the harness and its subprocess tree. It is idempotent.
//
// Close does not clear the launch marker: a terminated process tree is not the
// same fact as a confirmed-gone one, and this is the path a crashing caller
// never reaches anyway. Release is for a caller that has finished with an
// assignment and wants recovery to stop reserving against it.
func (s *Session) Close() { s.fail(ErrClosed) }

// Release closes the session and, once its harness is confirmed gone, removes
// the launch marker recovery would otherwise reserve against. It returns the
// reclamation outcome: a marker is only cleared on positive evidence, so a
// harness that cannot be confirmed terminated keeps its marker and its
// ErrUnreclaimed, which is what tells a later run to hold.
func (s *Session) Release(ctx context.Context) (Reclamation, error) {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	s.Close()
	if host == nil {
		return Reclamation{Confirmed: true}, nil
	}
	// Wait for this session's own harness to be reaped before asking the general
	// question. Reclaim reads a process group, and a process that has exited but
	// not yet been reaped still occupies one — so asking immediately after Close
	// reports an ordinary, orderly shutdown as an unresolved survivor, which in a
	// caller's hands becomes a worker that looks stuck every time it finishes.
	s.awaitReaped(ctx)
	dir := host.cfg.Dir
	out, err := Reclaim(ctx, dir)
	if err != nil || !out.Confirmed {
		return out, err
	}
	// The harness is gone, so its home is no longer being written to. If it
	// refreshed the login, return that to the source now rather than leaving the
	// next worker to rediscover an expired one.
	if s.options.Engine == Codex && s.options.RuntimeHome != "" {
		if shareErr := writeBackCredential(s.options.Home, s.options.RuntimeHome); shareErr != nil {
			return out, shareErr
		}
	}
	return out, clearLaunchRecord(dir)
}
func (s *Session) fail(err error) { s.failTurn(nil, err) }
func (s *Session) failTurn(expected *Turn, err error) {
	s.mu.Lock()
	if expected != nil {
		if s.active != expected {
			s.mu.Unlock()
			return
		}
		expected.mu.Lock()
		finished := expected.finished
		expected.mu.Unlock()
		if finished {
			s.mu.Unlock()
			return
		}
	}
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.failure == nil && err != nil && !errors.Is(err, ErrClosed) {
		s.failure = err
	}
	close(s.done)
	t := s.active
	w := s.transport
	host := s.tools
	s.mu.Unlock()
	if t != nil {
		t.finish("failed", err)
	}
	if w != nil {
		w.close()
	}
	if host != nil {
		host.close()
	}
}

// StartTurn begins one user turn. A session permits one active turn; queueing is
// caller policy. Cancelling ctx while active terminates the entire session to
// avoid orphaned model/tool work. Wait's own context does not cancel execution.
func (s *Session) StartTurn(ctx context.Context, in Input) (*Turn, error) {
	if err := s.lockOp(ctx); err != nil {
		return nil, err
	}
	defer s.unlockOp()
	return s.startTurn(ctx, in)
}
func (s *Session) startTurn(ctx context.Context, in Input) (*Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.Text == "" {
		return nil, errors.New("turn input is empty")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.active != nil {
		select {
		case <-s.active.done:
		default:
			s.mu.Unlock()
			return nil, ErrBusy
		}
	}
	t := &Turn{id: newID(), events: make(chan Event, s.options.EventBuffer), done: make(chan struct{})}
	t.starting = s.options.Engine == Codex
	s.active = t
	invalidate(&s.telemetry.Context.Observation, "conversation is changing; awaiting a fresh observation")
	t.result.Context = cloneContext(s.telemetry.Context)
	ref := s.ref
	s.mu.Unlock()
	var err error
	if s.options.Engine == Codex {
		err = s.startCodexTurn(ctx, t, ref, in)
	} else {
		err = s.transport.send(ctx, map[string]any{"type": "user", "session_id": ref.ID, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": in.Text}})
	}
	if err != nil {
		if definitiveRejection(err) {
			t.finish("rejected", err)
		} else {
			s.fail(err)
		}
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			s.failTurn(t, ctx.Err())
		case <-t.done:
		case <-s.done:
		}
	}()

	return t, nil
}

// startCodexTurn asks the server to open the turn and adopts the id it assigns.
// Notifications that arrived while the turn was still starting were buffered
// against the local id, so they are replayed here, under eventMu for the whole
// replay so no live notification interleaves with it, and with t.pending
// cleared under t.mu before the replay begins.
func (s *Session) startCodexTurn(ctx context.Context, t *Turn, ref Ref, in Input) error {
	p := map[string]any{"threadId": ref.ID, "input": []any{map[string]any{"type": "text", "text": in.Text}}}
	if s.options.Effort != "" {
		p["effort"] = s.options.Effort
	}
	body, err := s.transport.request(ctx, "turn/start", p)
	if err != nil {
		return err
	}
	var r struct {
		Turn struct{ ID string } `json:"turn"`
	}
	if json.Unmarshal(body, &r) != nil || r.Turn.ID == "" {
		return ErrProtocol
	}
	s.eventMu.Lock()
	t.mu.Lock()
	t.id = r.Turn.ID
	t.result.TurnID = r.Turn.ID
	t.starting = false
	pending := t.pending
	t.pending = nil
	t.pendingBytes = 0
	t.mu.Unlock()
	for _, event := range pending {
		s.notificationLocked(event)
	}
	s.eventMu.Unlock()
	return nil
}
func (s *Session) activeTurn(expected string) (*Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	t := s.active
	if t == nil || expected == "" || t.ID() != expected {
		return nil, ErrStaleTurn
	}
	select {
	case <-t.done:
		return nil, ErrStaleTurn
	default:
		return t, nil
	}
}

// Interrupt requests turn cancellation and waits for a terminal protocol event.
// Continue draining Turn.Events concurrently while Interrupt or Steer waits.
// Claude has no unique cancelled result code: a requested interrupt correlated
// with error_during_execution normalizes to interrupted, while NativeError stays
// true. Other error subtypes remain failures. This does not establish that the
// interruption was the sole cause, or that external tool effects stopped.
func (s *Session) Interrupt(ctx context.Context, expectedTurnID string) error {
	if err := s.lockOp(ctx); err != nil {
		return err
	}
	defer s.unlockOp()
	return s.interrupt(ctx, expectedTurnID)
}
func (s *Session) interrupt(ctx context.Context, expected string) error {
	t, err := s.activeTurn(expected)
	if err != nil {
		return err
	}
	method := "interrupt"
	p := map[string]any{}
	if s.options.Engine == Codex {
		method = "turn/interrupt"
		p["threadId"] = s.Ref().ID
		p["turnId"] = expected
	}
	t.mu.Lock()
	t.interruptRequested = true
	t.mu.Unlock()
	if _, err = s.transport.request(ctx, method, p); err != nil {
		t.mu.Lock()
		t.interruptRequested = false
		t.mu.Unlock()
		if !definitiveRejection(err) {
			s.fail(err)
		}
		return s.operationError("interrupt", err)
	}
	result, waitErr := t.Wait(ctx)
	if waitErr != nil {
		return waitErr
	}
	if result.Status != "interrupted" {
		return ErrStaleTurn
	}
	s.mu.Lock()
	s.caps.Interrupt = Capability{Native, "acknowledged interrupt and terminal turn event"}
	s.mu.Unlock()
	return nil
}

// Steer uses native Codex turn/steer or, on Claude, interrupt-and-continue. The
// composed path returns a NEW Turn after the old turn's terminal result. There
// is no silent retry/restart; transport loss returns an error for caller policy.
func (s *Session) Steer(ctx context.Context, expected string, in Input, o SteerOptions) (SteerResult, error) {
	if err := s.lockOp(ctx); err != nil {
		return SteerResult{}, err
	}
	defer s.unlockOp()
	t, err := s.activeTurn(expected)
	if err != nil {
		return SteerResult{}, err
	}
	if in.Text == "" {
		return SteerResult{}, errors.New("steering input is empty")
	}
	if s.options.Engine == Claude {
		if o.RequireNative {
			return SteerResult{}, &UnsupportedError{"steer", Capability{Composed, "Claude steering interrupts and starts another turn"}}
		}
		if err = s.interrupt(ctx, expected); err != nil {
			return SteerResult{}, err
		}
		next, err := s.startTurn(ctx, in)
		if err != nil {
			return SteerResult{}, err
		}
		s.mu.Lock()
		s.caps.Steer = Capability{Composed, "interrupt-and-continue succeeded in the installed harness"}
		s.mu.Unlock()
		return SteerResult{Composed, next}, nil
	}
	body, err := s.transport.request(ctx, "turn/steer", map[string]any{"threadId": s.Ref().ID, "expectedTurnId": expected, "input": []any{map[string]any{"type": "text", "text": in.Text}}})
	if err != nil {
		if !definitiveRejection(err) {
			s.fail(err)
		}
		return SteerResult{}, s.operationError("steer", err)
	}
	var reply struct {
		TurnID string `json:"turnId"`
	}
	if json.Unmarshal(body, &reply) != nil || reply.TurnID != expected {
		s.fail(ErrProtocol)
		return SteerResult{}, ErrProtocol
	}
	s.mu.Lock()
	s.caps.Steer = Capability{Native, "turn/steer acknowledged by installed harness"}
	s.mu.Unlock()
	return SteerResult{Native, t}, nil
}
func (s *Session) operationError(operation string, err error) error {
	if !errors.Is(err, ErrUnsupported) {
		return err
	}
	c := Capability{Unsupported, "method unavailable in installed harness"}
	s.mu.Lock()
	switch operation {
	case "compact":
		s.caps.Compact = c
	case "steer":
		s.caps.Steer = c
	case "interrupt":
		s.caps.Interrupt = c
	}
	s.mu.Unlock()
	return &UnsupportedError{operation, c}
}

func (s *Session) lockOp(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.opGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlockOp()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	}
}
func (s *Session) unlockOp() { <-s.opGate }

func definitiveRejection(err error) bool {
	return errors.Is(err, ErrRejected) || errors.Is(err, ErrUnsupported)
}
