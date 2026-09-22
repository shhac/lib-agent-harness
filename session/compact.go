package session

import (
	"context"
	"encoding/json"
)

// Compact requests native manual context compaction in an idle session. The
// returned Turn remains active until the native terminal event, not just the
// request acknowledgement. Its ID is empty until turn/started supplies it.
// Drain Events concurrently; cancellation closes the session like StartTurn.
// Claude is unsupported: sending a summarization prompt is not compaction.
func (s *Session) Compact(ctx context.Context) (*Turn, error) {
	if err := s.lockOp(ctx); err != nil {
		return nil, err
	}
	defer s.unlockOp()
	s.mu.Lock()
	if s.options.Engine != Codex && !s.closed {
		c := s.caps.Compact
		s.mu.Unlock()
		return nil, &UnsupportedError{"compact", c}
	}
	// Compaction is not a turn that authorizes tool work, so unlike StartTurn it
	// neither waits for hosted tools to settle nor reopens their channel.
	if err := s.claimIdleLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	t := &Turn{events: make(chan Event, s.options.EventBuffer), done: make(chan struct{}), starting: true, compacting: true, awaitingCompactID: true}
	s.active = t
	ref := s.ref
	s.mu.Unlock()
	body, err := s.transport.request(ctx, "thread/compact/start", map[string]any{"threadId": ref.ID})
	if err != nil {
		if definitiveRejection(err) {
			t.finish("rejected", err)
		} else {
			s.fail(err)
		}
		return nil, s.operationError("compact", err)
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(body, &reply) != nil || reply == nil {
		s.fail(ErrProtocol)
		return nil, ErrProtocol
	}
	s.invalidateContext(t, "context compaction requested; awaiting a fresh observation")
	s.mu.Lock()
	s.caps.Compact = Capability{Native, "thread/compact/start acknowledged by installed harness"}
	s.mu.Unlock()
	// The id arrives later, with turn/started.
	s.replayStarting(t, "")
	s.watchTurn(ctx, t)
	return t, nil
}

func (s *Session) compactTurnStarted(t *Turn, p map[string]json.RawMessage) {
	t.mu.Lock()
	eligible := t.compacting && t.awaitingCompactID
	t.mu.Unlock()
	if !eligible {
		return
	}
	var turn struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(p["turn"], &turn) != nil || turn.ID == "" {
		s.fail(ErrProtocol)
		return
	}
	t.mu.Lock()
	if !t.compacting || !t.awaitingCompactID {
		t.mu.Unlock()
		return
	}
	t.id = turn.ID
	t.result.TurnID = turn.ID
	t.awaitingCompactID = false
	t.mu.Unlock()
	s.emit(t, Event{Kind: "status", Status: "compacting"})
}
