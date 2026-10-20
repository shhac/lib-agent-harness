package session

// fx's session/update dialect, and the prompt response that ends a turn.
//
// Kept apart from the other engines because fx reports the context window
// and the occupancy in one notification, and because its tool calls name the
// tool in the update itself, without a separate meta block. Every shape here
// was observed from fx 0.0.14.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	harness "github.com/shhac/lib-agent-harness"
)

// Turn failure codes an fx prompt response can end with, beside its own
// enumerated stop reasons and the refusal codes in fx.go.
const (
	fxCancelled        = "cancelled"
	fxStopUnrecognized = "stop_reason_unrecognized"
)

// fxEvent routes one notification for the active turn.
func (s *Session) fxEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if str(m, "method") != "session/update" {
		return
	}
	var p struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(m["params"], &p) != nil || p.Update == nil || ref.ID == "" || p.SessionID != ref.ID {
		return
	}
	switch str(p.Update, "sessionUpdate") {
	case "agent_message_chunk":
		var content struct{ Type, Text string }
		if json.Unmarshal(p.Update["content"], &content) == nil && content.Type == "text" && content.Text != "" {
			s.text(t, fxMessageItem(t, p.Update), content.Text, false)
		}
	case "tool_call":
		id := str(p.Update, "toolCallId")
		if id == "" {
			return
		}
		// Text after a tool call is a new message: the turn's result is the
		// text that follows the last one.
		t.mu.Lock()
		t.fxResponse++
		t.mu.Unlock()
		status := str(p.Update, "status")
		if status == "" {
			status = "pending"
		}
		s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: id, Tool: fxToolName(p.Update), Status: status}, p.Update["rawInput"], ""))
	case "tool_call_update":
		id, status := str(p.Update, "toolCallId"), str(p.Update, "status")
		if id != "" && (status == "completed" || status == "failed") {
			s.emit(t, withImages(s.withToolPayload(Event{Kind: "tool_completed", ItemID: id, Status: status}, p.Update["rawInput"], grokToolOutput(p.Update)), grokToolImages(p.Update)))
		}
	case "usage_update":
		s.fxContext(t, p.Update)
	}
}

// fxMessageItem names the text item a message chunk belongs to. fx gives each
// message a stable id, which is used when it has one; otherwise the chunks
// of a turn share one item.
func fxMessageItem(t *Turn, update map[string]json.RawMessage) string {
	if id := str(update, "messageId"); id != "" {
		return id
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return "response-" + strconv.Itoa(t.fxResponse)
}

// fxToolName is the tool's own identifier, never its title, which carries
// paths and commands.
func fxToolName(update map[string]json.RawMessage) string {
	if name := str(update, "name"); name != "" {
		return name
	}
	return "tool"
}

// fxContext publishes the context the session occupies, and the window it
// occupies it in. fx states both in one update, so the occupancy is measured
// rather than estimated.
func (s *Session) fxContext(t *Turn, update map[string]json.RawMessage) {
	var u struct {
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
	}
	if json.Unmarshal(mustMarshal(update), &u) != nil || u.Used == nil || *u.Used < 0 {
		return
	}
	s.mu.Lock()
	model, capacity := s.fxModel, s.fxCapacity
	s.mu.Unlock()
	if capacity == 0 && u.Size != nil && *u.Size > 0 {
		capacity = *u.Size
		s.mu.Lock()
		s.fxCapacity = capacity
		s.mu.Unlock()
	}
	used := *u.Used
	c := ContextSnapshot{Model: model, UsedTokens: &used}
	c.Observation = observation("usage_update", harness.Measured)
	c.Reason = "fx's own count of the conversation's context"
	if u.Size != nil && *u.Size > 0 {
		size := *u.Size
		c.CapacityTokens = &size
		c.ModelCapacityTokens = cloneValue(&size)
	}
	setContextPercent(&c)
	s.observeContext(c, t)
}

// startFxTurn sends the prompt. Its response is the end of the turn, so it
// is awaited apart from the request that started it.
func (s *Session) startFxTurn(request, lifetime context.Context, t *Turn, ref Ref, in Input) error {
	params := grokPromptParams(ref.ID, in.Text)
	w, ok := s.transport.(asyncWire)
	if !ok {
		go func() {
			body, err := s.transport.request(lifetime, "session/prompt", params)
			s.fxPromptEnded(t, body, err)
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
			s.fxPromptEnded(t, r.body, r.err)
		case <-t.done:
		case <-s.done:
		}
	}()
	return nil
}

// fxPromptEnded ends the turn from the prompt's response. It holds the event
// lock so the turn ends after every notification that preceded the response.
func (s *Session) fxPromptEnded(t *Turn, body json.RawMessage, err error) {
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
		s.fxTurnEnded(t, "failed", refusal.code, refusal.HarnessFacts().Cause, Usage{})
		return
	}
	var r struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		s.failTurn(t, ErrProtocol)
		return
	}
	t.mu.Lock()
	interrupted := t.interruptRequested
	t.mu.Unlock()
	status, code := fxOutcome(r.StopReason, interrupted)
	s.fxTurnEnded(t, status, code, "", parseFxTurnUsage(r.Usage))
}

// fxOutcome maps a stop reason to a turn status and, for a failure, a fixed
// code. Only end_turn completes a turn. A cancellation is an interruption only
// when one was requested; otherwise it is a refused permission or a
// cancellation from elsewhere, and both are failures.
func fxOutcome(stop string, interruptRequested bool) (string, string) {
	switch stop {
	case "end_turn":
		return "completed", ""
	case "cancelled":
		if interruptRequested {
			return "interrupted", ""
		}
		return "failed", fxCancelled
	case "max_tokens", "max_turn_requests", "refusal":
		return "failed", stop
	}
	return "failed", fxStopUnrecognized
}

// fxTurnEnded publishes the turn's own accounting, when fx stated it, and
// its status, then ends it. A refused prompt carries the cause its refusal
// was classified with.
func (s *Session) fxTurnEnded(t *Turn, status, code string, cause harness.Cause, usage Usage) {
	var err error
	t.mu.Lock()
	if usage.Known {
		usage.Final = true
		t.result.Usage = usage
	}
	if code != "" {
		t.result.NativeError = true
		err = &TurnError{Engine: harness.Fx, Code: code, Cause: cause}
	}
	t.mu.Unlock()
	if usage.Known {
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
}

// parseFxTurnUsage reads the turn's accounting from a prompt response. Its
// inputTokens already counts cached input: on fx 0.0.14 a turn's input and
// output match the context its usage_update then states, with the cache reads
// inside that input. Reasoning tokens are part of the output.
func parseFxTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Read      *int64 `json:"cacheReadTokens"`
		Write     *int64 `json:"cacheWriteTokens"`
		Reasoning *int64 `json:"reasoningTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	read, write, reasoning := valueOr(c.Read), valueOr(c.Write), valueOr(c.Reasoning)
	input, output := *c.Input, *c.Output
	if input < 0 || output < 0 || read < 0 || write < 0 || reasoning < 0 || read > input || write > input-read || reasoning > output {
		return Usage{}
	}
	return Usage{Usage: harness.Usage{Known: true, Input: input, Output: output, CacheRead: read, CacheWrite: write, Reasoning: reasoning, CacheKnown: c.Read != nil && c.Write != nil, ReasoningKnown: c.Reasoning != nil}}
}
