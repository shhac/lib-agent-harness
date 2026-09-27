package session

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"

	harness "github.com/shhac/lib-agent-harness"
)

// Grok's session/update dialect, and the prompt response that ends a turn.
// Kept apart from the other engines because its usage arrives in two scopes
// that must not be confused: each model response's figures stream as a
// response_completed notification, and the turn's own accounting is in the
// prompt's response. Every shape here was observed from grok 1.0.41.

// Turn failure codes a Grok prompt response can end with, beside its own
// enumerated stop reasons and the refusal codes in grok.go.
const (
	grokPermissionRejected = "permission_rejected"
	grokCancelled          = "cancelled"
	grokStopUnrecognized   = "stop_reason_unrecognized"
)

// grokEvent routes one notification for the active turn. Streamed content is
// ACP's session/update; per-response usage is Grok's own extension envelope.
func (s *Session) grokEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	method := str(m, "method")
	if method != "session/update" && method != "_x.ai/session_notification" {
		return
	}
	var p struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(m["params"], &p) != nil || p.Update == nil || ref.ID == "" || p.SessionID != ref.ID {
		return
	}
	kind := str(p.Update, "sessionUpdate")
	if method == "_x.ai/session_notification" {
		if kind == "response_completed" {
			s.grokResponseCompleted(t, p.Update)
		}
		return
	}
	switch kind {
	case "agent_message_chunk":
		var content struct{ Type, Text string }
		if json.Unmarshal(p.Update["content"], &content) == nil && content.Type == "text" && content.Text != "" {
			t.mu.Lock()
			item := "response-" + strconv.Itoa(t.grokResponse)
			t.mu.Unlock()
			s.text(t, item, content.Text, false)
		}
	case "tool_call":
		id := str(p.Update, "toolCallId")
		if id == "" {
			return
		}
		status := str(p.Update, "status")
		if status == "" {
			status = "pending"
		}
		s.emit(t, Event{Kind: "tool_started", ItemID: id, Tool: grokToolName(p.Update), Status: status})
	case "tool_call_update":
		id, status := str(p.Update, "toolCallId"), str(p.Update, "status")
		if id != "" && (status == "completed" || status == "failed") {
			s.emit(t, Event{Kind: "tool_completed", ItemID: id, Status: status})
		}
	}
}

// grokResponseCompleted publishes one model response's usage while the turn
// runs, estimates the context from it, and closes that response's text.
func (s *Session) grokResponseCompleted(t *Turn, update map[string]json.RawMessage) {
	usage := parseGrokResponseUsage(update["usage"])
	s.observeRequestUsage(t, usage)
	s.grokContext(t, usage)
	t.mu.Lock()
	t.grokResponse++
	t.mu.Unlock()
}

// grokContext estimates occupancy from the latest response's whole input, and
// states capacity from the window Grok's model state gives for the session's
// model. Pending output and tool results are not counted, hence Estimated.
func (s *Session) grokContext(t *Turn, u Usage) {
	if !u.Known {
		return
	}
	s.mu.Lock()
	model, capacity := s.grokModel, s.grokCapacity
	s.mu.Unlock()
	used := u.Input
	c := ContextSnapshot{Model: model, UsedTokens: &used}
	c.Observation = observation("response_completed.usage", harness.Estimated)
	c.Reason = "latest model input; pending output and tool results are not counted"
	if capacity > 0 {
		c.CapacityTokens = &capacity
		c.ModelCapacityTokens = cloneValue(&capacity)
	}
	setContextPercent(&c)
	s.observeContext(c, t)
}

var grokToolIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// grokToolName is the tool's own identifier, never its title, which carries
// paths and commands.
func grokToolName(update map[string]json.RawMessage) string {
	var meta struct {
		Tool struct {
			Name string `json:"name"`
		} `json:"x.ai/tool"`
	}
	_ = json.Unmarshal(update["_meta"], &meta)
	for _, name := range []string{meta.Tool.Name, str(update, "title")} {
		if grokToolIdentifier.MatchString(name) {
			return name
		}
	}
	return "tool"
}

// startGrokTurn sends the prompt. Its response is the end of the turn, so it
// is awaited apart from the request that started it; a transport that cannot
// send now and answer later waits for the whole exchange instead.
func (s *Session) startGrokTurn(request, lifetime context.Context, t *Turn, ref Ref, in Input) error {
	params := grokPromptParams(ref.ID, in.Text)
	w, ok := s.transport.(asyncWire)
	if !ok {
		go func() {
			body, err := s.transport.request(lifetime, "session/prompt", params)
			s.grokPromptEnded(t, body, err)
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
			s.grokPromptEnded(t, r.body, r.err)
		case <-t.done:
		case <-s.done:
		}
	}()
	return nil
}

// grokPromptEnded ends the turn from the prompt's response. It holds the event
// lock so the turn ends after every notification that preceded the response.
func (s *Session) grokPromptEnded(t *Turn, body json.RawMessage, err error) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if t.ended() {
		return
	}
	if err != nil {
		var refusal *grokRefusal
		if !errors.As(err, &refusal) {
			s.failTurn(t, err)
			return
		}
		// A refused prompt is a definitive answer about this turn, not about the
		// session, which stays usable. Tools may already have run.
		s.grokTurnEnded(t, "failed", refusal.code, Usage{})
		return
	}
	var r struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Usage    json.RawMessage `json:"usage"`
			Category string          `json:"cancellationCategory"`
		} `json:"_meta"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		s.failTurn(t, ErrProtocol)
		return
	}
	t.mu.Lock()
	interrupted := t.interruptRequested
	t.mu.Unlock()
	status, code := grokOutcome(r.StopReason, r.Meta.Category, interrupted)
	s.grokTurnEnded(t, status, code, parseGrokTurnUsage(r.Meta.Usage))
}

// grokOutcome maps a stop reason to a turn status and, for a failure, a fixed
// code. Only end_turn completes a turn. A cancellation is an interruption only
// when one was requested; otherwise it is a refused permission or a
// cancellation from elsewhere, and both are failures.
func grokOutcome(stop, category string, interruptRequested bool) (string, string) {
	switch stop {
	case "end_turn":
		return "completed", ""
	case "cancelled":
		switch {
		case interruptRequested:
			return "interrupted", ""
		case category == "PermissionRejected":
			return "failed", grokPermissionRejected
		}
		return "failed", grokCancelled
	case "max_tokens", "max_turn_requests", "refusal":
		return "failed", stop
	}
	return "failed", grokStopUnrecognized
}

// grokTurnEnded publishes the turn's own accounting, when Grok stated it, and
// its status, then ends it.
func (s *Session) grokTurnEnded(t *Turn, status, code string, usage Usage) {
	var err error
	t.mu.Lock()
	if usage.Known {
		usage.Final = true
		t.result.Usage = usage
	}
	if code != "" {
		t.result.NativeError = true
		err = &TurnError{Engine: harness.Grok, Code: code}
	}
	t.mu.Unlock()
	if usage.Known {
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
}

// parseGrokTurnUsage reads the turn's accounting from a prompt response. Its
// inputTokens already counts cached input: across every observed turn it
// equals the sum of that turn's responses' uncached input and cache reads.
func parseGrokTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Read      *int64 `json:"cachedReadTokens"`
		Write     *int64 `json:"cacheCreationTokens"`
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
	return Usage{Usage: harness.Usage{Known: true, Input: input, Output: output, CacheRead: read, CacheWrite: write, Reasoning: reasoning, CacheKnown: c.Read != nil && c.Write != nil}}
}

// parseGrokResponseUsage reads one response's figures, whose input_tokens
// excludes both cache figures, into the shared shape, whose Input includes
// them.
func parseGrokResponseUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"input_tokens"`
		Output    *int64 `json:"output_tokens"`
		Read      *int64 `json:"cache_read_input_tokens"`
		Write     *int64 `json:"cache_creation_input_tokens"`
		Reasoning *int64 `json:"reasoning_tokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	read, write, reasoning := valueOr(c.Read), valueOr(c.Write), valueOr(c.Reasoning)
	input, output := *c.Input, *c.Output
	if input < 0 || output < 0 || read < 0 || write < 0 || reasoning < 0 || reasoning > output || input > maxInt64-read || input+read > maxInt64-write {
		return Usage{}
	}
	return Usage{Usage: harness.Usage{Known: true, Input: input + read + write, Output: output, CacheRead: read, CacheWrite: write, Reasoning: reasoning, CacheKnown: c.Read != nil && c.Write != nil}}
}

func valueOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
