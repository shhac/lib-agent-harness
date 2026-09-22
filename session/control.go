package session

// Turn control: starting turns, interrupting and steering them. Each operation
// holds the session's operation gate, so two controls never interleave on one
// conversation.

import (
	"context"
	"encoding/json"
	"errors"
)

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
	return s.startTurnScoped(ctx, ctx, in)
}

// startTurnScoped separates the two lifetimes a turn actually has.
//
// request bounds getting the turn started — a send, or a protocol round trip.
// lifetime is what the turn is watched against, and cancelling it stops the
// session, so it must be something that outlives the turn. For an ordinary
// StartTurn the caller supplies both and they are the same context. For a
// composed steer they are emphatically not: the control request is bounded, and
// binding the replacement turn to it would end the session the moment steering
// returned — which is precisely the trap this exists to close.
func (s *Session) startTurnScoped(lifetime, request context.Context, in Input) (*Turn, error) {
	if err := request.Err(); err != nil {
		return nil, err
	}
	if err := lifetime.Err(); err != nil {
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
	// Starting a turn is what authorizes tool work again after a pause. Anything
	// queued from before is refused by the generation change, and anything still
	// outstanding stops this turn from starting at all: a cancelled call is not a
	// stopped one, and the next turn must not run against a workspace the previous
	// one may still be writing to. Both take only the host's own lock, so they are
	// safe from inside the session's.
	if err := s.tools.readyForWork(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.tools.reopen()
	host := s.tools
	t := &Turn{id: newID(), events: make(chan Event, s.options.EventBuffer), done: make(chan struct{}), closeTools: host.closeAdmission}
	t.starting = s.options.Engine == Codex
	s.active = t
	invalidate(&s.telemetry.Context.Observation, "conversation is changing; awaiting a fresh observation")
	t.result.Context = cloneContext(s.telemetry.Context)
	ref := s.ref
	s.mu.Unlock()
	var err error
	if s.options.Engine == Codex {
		err = s.startCodexTurn(request, t, ref, in)
	} else {
		err = s.transport.send(request, claudeUserFrame(ref.ID, in.Text))
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
		case <-lifetime.Done():
			s.failTurn(t, lifetime.Err())
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
	body, err := s.transport.request(ctx, "turn/start", codexTurnParams(ref.ID, in.Text, s.options.Effort))
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
		// The interrupted turn is over, but the caller's tools are not: both
		// installed harnesses were observed reporting a terminal interrupted
		// result while a hosted call was still running. Cancelling them is a
		// request, not an outcome, so the replacement turn waits for the handlers
		// to actually return. If they do not within the caller's context, no
		// replacement is started — continuing would be describing a workspace that
		// is still moving as one that has stopped.
		s.CancelTools()
		if err = s.AwaitToolsSettled(ctx); err != nil {
			return SteerResult{}, err
		}
		lifetime := s.lifetime
		if lifetime == nil {
			lifetime = context.Background()
		}
		next, err := s.startTurnScoped(lifetime, ctx, in)
		if err != nil {
			return SteerResult{}, err
		}
		s.mu.Lock()
		s.caps.Steer = Capability{Composed, "interrupt-and-continue succeeded in the installed harness"}
		s.mu.Unlock()
		return SteerResult{Composed, next}, nil
	}
	body, err := s.transport.request(ctx, "turn/steer", map[string]any{"threadId": s.Ref().ID, "expectedTurnId": expected, "input": codexInput(in.Text)})
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
