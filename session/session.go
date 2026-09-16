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
	transport wire
	active    *Turn
	closed    bool
	done      chan struct{}
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
	s := &Session{options: o, ref: reference(o, ""), caps: CapabilitiesFor(o.Engine), done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	if r != nil {
		s.ref = *r
	} else if o.Engine == Claude {
		s.ref.ID = newID()
	}
	// Reader callbacks may fire before startup returns; the transport assignment
	// is protected so an early process failure cannot race Close.
	s.mu.Lock()
	w, err := newProcessWire(ctx, o, s.ref.ID, r != nil, s.notification, s.fail)
	s.transport = w
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err = s.initialize(ctx, r != nil); err != nil {
		s.fail(err)
		return nil, err
	}
	return s, nil
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
		if _, err := s.transport.request(ctx, "initialize", map[string]any{}); err != nil {
			return err
		}
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
func (s *Session) Close()         { s.fail(ErrClosed) }
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
	close(s.done)
	t := s.active
	w := s.transport
	s.mu.Unlock()
	if t != nil {
		t.finish("failed", err)
	}
	if w != nil {
		w.close()
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
