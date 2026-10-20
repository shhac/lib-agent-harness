package session

import (
	"context"
	"encoding/json"
	"errors"

	harness "github.com/shhac/lib-agent-harness"
)

// The turn lifecycle every ACP engine shares: a turn is one session/prompt
// request, and its response ends the turn. Where the response states usage,
// and which stop reasons it adds, are the engine's own, so each passes a
// reader for its response.

// Turn failure codes for the stop reasons every ACP agent can end with,
// beside the enumerated ones (max_tokens, max_turn_requests, refusal) that are
// their own codes.
const (
	acpCancelled        = "cancelled"
	acpStopUnrecognized = "stop_reason_unrecognized"
)

// acpTurnEnd is what a prompt response says about the turn it ends: its
// status, a fixed code when it failed, and the turn's own usage when the
// engine stated it.
type acpTurnEnd struct {
	status, code string
	usage        Usage
}

// acpPromptReader reads an engine's prompt response, reporting false for one
// that is not a prompt response.
type acpPromptReader func(body json.RawMessage, interruptRequested bool) (acpTurnEnd, bool)

func acpPromptParams(session, text string) map[string]any {
	return map[string]any{"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": text}}}
}

// acpStartTurn is an ACP engine's startTurn: it sends the prompt and ends the
// turn from the response read.
func acpStartTurn(engine harness.Engine, read acpPromptReader) func(s *Session, request, lifetime context.Context, t *Turn, ref Ref, in Input) error {
	return func(s *Session, request, lifetime context.Context, t *Turn, ref Ref, in Input) error {
		return s.startACPTurn(request, lifetime, t, ref, in, engine, read)
	}
}

// startACPTurn sends the prompt. Its response is the end of the turn, so it
// is awaited apart from the request that started it; a transport that cannot
// send now and answer later waits for the whole exchange instead.
func (s *Session) startACPTurn(request, lifetime context.Context, t *Turn, ref Ref, in Input, engine harness.Engine, read acpPromptReader) error {
	params := acpPromptParams(ref.ID, in.Text)
	w, ok := s.transport.(asyncWire)
	if !ok {
		go func() {
			body, err := s.transport.request(lifetime, "session/prompt", params)
			s.acpPromptEnded(t, engine, read, body, err)
		}()
		return nil
	}
	reply, forget, err := w.call(request, "session/prompt", params)
	if err != nil {
		return err
	}
	go func() {
		defer forget()
		select {
		case r := <-reply:
			s.acpPromptEnded(t, engine, read, r.body, r.err)
		case <-t.done:
		case <-s.done:
		}
	}()
	return nil
}

// acpPromptEnded ends the turn from the prompt's response. It holds the event
// lock so the turn ends after every notification that preceded the response.
func (s *Session) acpPromptEnded(t *Turn, engine harness.Engine, read acpPromptReader, body json.RawMessage, err error) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if t.ended() {
		return
	}
	if err != nil {
		var refusal *acpRefusal
		if !errors.As(err, &refusal) {
			s.failTurn(t, err)
			return
		}
		// A refused prompt is a definitive answer about this turn, not about the
		// session, which stays usable. Tools may already have run.
		s.acpTurnEnded(t, engine, acpTurnEnd{status: "failed", code: refusal.code}, refusal.HarnessFacts().Cause)
		return
	}
	t.mu.Lock()
	interrupted := t.interruptRequested
	t.mu.Unlock()
	end, ok := read(body, interrupted)
	if !ok {
		s.failTurn(t, ErrProtocol)
		return
	}
	s.acpTurnEnded(t, engine, end, "")
}

// acpOutcome maps a stop reason to a turn status and, for a failure, a fixed
// code. Only end_turn completes a turn. A cancellation is an interruption only
// when one was requested; otherwise it came from elsewhere, and is a failure.
func acpOutcome(stop string, interruptRequested bool) (string, string) {
	switch stop {
	case "end_turn":
		return "completed", ""
	case "cancelled":
		if interruptRequested {
			return "interrupted", ""
		}
		return "failed", acpCancelled
	case "max_tokens", "max_turn_requests", "refusal":
		return "failed", stop
	}
	return "failed", acpStopUnrecognized
}

// acpTurnEnded publishes the turn's own accounting, when the engine stated
// it, and its status, then ends it. A refused prompt carries the cause its
// refusal was classified with.
func (s *Session) acpTurnEnded(t *Turn, engine harness.Engine, end acpTurnEnd, cause harness.Cause) {
	var err error
	usage := end.usage
	t.mu.Lock()
	if usage.Known {
		usage.Final = true
		t.result.Usage = usage
	}
	if end.code != "" {
		t.result.NativeError = true
		err = &TurnError{Engine: engine, Code: end.code, Cause: cause}
	}
	t.mu.Unlock()
	if usage.Known {
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: end.status})
	t.finish(end.status, err)
}
