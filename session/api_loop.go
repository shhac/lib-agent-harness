package session

// The turn loop the library runs for an OpenAI-compatible session.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
)

// startAPITurn records the turn and its input, then runs the loop in the
// background. The turn outlives the request that started it; interrupting it
// or closing the session is what stops it.
func (s *Session) startAPITurn(t *Turn, in Input) error {
	a := s.api
	ctx, cancel := context.WithCancel(context.Background())
	done, err := a.begin(cancel)
	if err != nil {
		cancel()
		return err
	}
	for _, r := range []record{{Type: recordTurnStart, Turn: t.id}, {Type: recordUser, Turn: t.id, Text: in.Text}} {
		if err = a.append(r); err != nil {
			cancel()
			close(done)
			return err
		}
	}
	s.touch()
	go func() {
		defer close(done)
		defer cancel()
		s.runAPITurn(ctx, t)
	}()
	return nil
}

// turnAccount is a turn's own usage: the sum of its responses while every
// request that may have been billed reported its figures, unknown otherwise.
type turnAccount struct {
	total Usage
	known bool
}

func (u *turnAccount) add(next harness.Usage) {
	if !next.Known {
		u.known = false
		return
	}
	u.total = u.total.add(Usage{Usage: next})
	if !u.total.Known {
		u.known = false
	}
}

// runAPITurn calls the model until it answers without calls, the step bound
// is reached, a request fails, or the turn is interrupted.
func (s *Session) runAPITurn(ctx context.Context, t *Turn) {
	a := s.api
	account := &turnAccount{known: true}
	for step := 1; ; step++ {
		if step > s.options.Loop.MaxSteps {
			s.endAPITurn(t, "failed", &TurnError{Engine: harness.OpenAICompatible, Code: TurnStepLimit}, account)
			return
		}
		result, err := a.complete(ctx, s.apiConfig(), a.messages(), a.tools)
		if mayHaveBilled(err) {
			account.add(result.Usage)
			if a.append(record{Type: recordUsage, Turn: t.id, Usage: &result.Usage}) != nil {
				return
			}
			s.observeAPIUsage(t, result.Usage)
		}
		if err != nil {
			if ctx.Err() != nil {
				s.endAPITurn(t, "interrupted", nil, account)
				return
			}
			s.endAPITurn(t, "failed", apiTurnFailure(err), account)
			return
		}
		response := a.nextResponse()
		message := result.Message
		if a.append(record{Type: recordAssistant, Turn: t.id, Response: response, Text: message.Content, Calls: message.ToolCalls, Replay: message.Replay}) != nil {
			return
		}
		s.touch()
		if message.Content != "" {
			s.text(t, "response-"+strconv.Itoa(response), message.Content, true)
		}
		if t.ended() {
			// The text overran its limit and ended the session.
			s.endAPITurn(t, "interrupted", nil, account)
			return
		}
		if len(message.ToolCalls) == 0 {
			s.endAPITurn(t, "completed", nil, account)
			return
		}
		for i, call := range message.ToolCalls {
			if ctx.Err() != nil || !s.runAPICall(ctx, t, response, call) {
				s.skipCalls(t, response, message.ToolCalls[i:])
				s.endAPITurn(t, "interrupted", nil, account)
				return
			}
		}
		// A closing tool ended the work. The turn ends with it, rather than
		// asking the model to carry on against a closed channel.
		if s.tools.channelClosed() {
			s.endAPITurn(t, "completed", nil, account)
			return
		}
	}
}

// runAPICall records a call, then runs it through the tool host, which
// serializes it, applies the closing-tool rule and tracks its settlement. It
// reports false when the turn was stopped while the call was running: the
// handler has been asked to stop, and its result is recorded when it returns.
func (s *Session) runAPICall(ctx context.Context, t *Turn, response int, call completion.ToolCall) bool {
	a := s.api
	name := call.Function.Name
	if a.append(record{Type: recordToolCall, Turn: t.id, Response: response, Call: call.ID, Tool: name}) != nil {
		return false
	}
	s.touch()
	s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: call.ID, Tool: name, Status: "running"}, json.RawMessage(call.Function.Arguments), ""))
	settle := func(out toolOutcome) {
		outcome := outcomeRan
		if !out.ran {
			outcome = outcomeRefused
		}
		if out.unknown {
			outcome = outcomeUnknown
		}
		_ = a.append(record{Type: recordToolResult, Turn: t.id, Response: response, Call: call.ID, Tool: name, Text: out.text, IsError: out.isError, Outcome: outcome})
	}
	// Admission happens here, synchronously, so a call the loop hands over is
	// registered with the host before the loop can observe an interruption.
	ready, refusal := s.tools.prepareCall(t.id+":"+strconv.Itoa(response)+":"+call.ID, name, json.RawMessage(call.Function.Arguments))
	outcomes := make(chan toolOutcome, 1)
	if refusal != nil {
		settle(*refusal)
		outcomes <- *refusal
	} else {
		go func() { outcomes <- s.tools.execute(ready, t.id, settle) }()
	}
	select {
	case out := <-outcomes:
		s.touch()
		status := "completed"
		switch {
		case !out.ran:
			status = "refused"
			s.emit(t, Event{Kind: "tool_refused", Tool: name, Status: out.reason})
		case out.isError:
			status = "failed"
		}
		// Output is what the model was answered: the handler's bounded result,
		// its error, or the library's refusal.
		event := Event{Kind: "tool_completed", ItemID: call.ID, Tool: name, Status: status}
		if out.code != "" {
			facts := (&TurnError{Engine: harness.OpenAICompatible, Code: out.code}).HarnessFacts()
			event.ErrorFacts = &facts
		}
		s.emit(t, s.withToolPayload(event, nil, out.text))
		return true
	case <-ctx.Done():
		// Workspace handlers bound their cancellation wait. Retain the turn
		// until that handler settles or fails the session with a stuck worker.
		if a.workspace != nil && isWorkbenchTool(name) {
			<-outcomes
		}
		return false
	}
}

// skipCalls answers the calls a stopped turn never reached as not run, so the
// history stays valid and the model is told nothing happened. A call still
// running when the turn stopped is not among them; its own result follows.
func (s *Session) skipCalls(t *Turn, response int, calls []completion.ToolCall) {
	a := s.api
	a.mu.Lock()
	_, started := callStates(a.records)
	a.mu.Unlock()
	for _, call := range calls {
		if started[record{Response: response, Call: call.ID}.key()] {
			continue
		}
		if a.append(record{Type: recordToolResult, Turn: t.id, Response: response, Call: call.ID, Tool: call.Function.Name, Text: bound(notRunText, a.resultLimit), IsError: true, Outcome: outcomeNotRun}) != nil {
			return
		}
	}
}

// endAPITurn records how the turn ended and publishes its accounting and
// status. A turn the session's closing already ended is still recorded, as
// interrupted, unless a workspace worker failed to settle, so a later
// resume finds the durable failure.
func (s *Session) endAPITurn(t *Turn, status string, err error, account *turnAccount) {
	code := ""
	var failure *TurnError
	if errors.As(err, &failure) {
		code = failure.Code
	}
	s.mu.Lock()
	sessionFailure := s.failure
	s.mu.Unlock()
	if s.api.workspace != nil && s.api.workspace.files.Stuck() || workspaceStuck(sessionFailure) {
		status, code = "failed", WorkspaceIOStuck
	} else if t.ended() {
		status, code = "interrupted", "session_closed"
	} else {
		t.mu.Lock()
		interrupted := t.interruptRequested
		t.mu.Unlock()
		if status == "interrupted" && !interrupted {
			// Nothing asked for this, so it was the session stopping.
			code = "session_closed"
		}
	}
	if s.api.append(record{Type: recordTurnEnd, Turn: t.id, Status: status, Code: code}) != nil || t.ended() {
		return
	}
	if account.known {
		usage := account.total
		usage.Known, usage.Final = true, true
		t.mu.Lock()
		t.result.Usage = usage
		t.mu.Unlock()
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	if failure != nil && failure.Code != TurnStepLimit {
		t.mu.Lock()
		t.result.NativeError = true
		t.mu.Unlock()
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
}

// observeAPIUsage publishes one response's usage, and estimates the context
// from its input: Chat Completions resends the whole history, so a response's
// input is the conversation's size when it was sent. No window is stated.
func (s *Session) observeAPIUsage(t *Turn, u harness.Usage) {
	if !u.Known {
		return
	}
	s.observeRequestUsage(t, Usage{Usage: u})
	used := u.Input
	c := ContextSnapshot{Model: s.options.Model, UsedTokens: &used}
	c.Observation = observation("chat.completion.usage", harness.Estimated)
	c.Reason = "latest request's input; the endpoint states no context window"
	s.observeContext(c, t)
}

// mayHaveBilled reports whether a request may have reached the provider. Only
// a refusal made before sending is known not to have.
func mayHaveBilled(err error) bool {
	var request *completion.RequestError
	if errors.As(err, &request) && request.Phase == completion.PhasePreflight {
		return false
	}
	return true
}

// apiTurnFailure carries a failed request's own facts into the turn's error.
func apiTurnFailure(err error) *TurnError {
	failure := &TurnError{Engine: harness.OpenAICompatible, Code: "request_failed", Cause: harness.CauseUnknown}
	var request *completion.RequestError
	if !errors.As(err, &request) {
		return failure
	}
	if request.Code != "" {
		failure.Code = request.Code
	}
	failure.Cause, failure.Phase, failure.RetryAfter, failure.ResetsAt = request.Cause, string(request.Phase), request.RetryAfter, request.ResetsAt
	return failure
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastEvent = time.Now().UTC()
	s.mu.Unlock()
}
