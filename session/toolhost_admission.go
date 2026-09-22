package session

// Whether a hosted tool call may run, and accounting for it until it stops.
// Every refusal is decided in one barrier, calls execute one at a time, and
// settlement is tracked so a paused channel can be awaited rather than polled.

import (
	"context"
	"encoding/json"
	"errors"
)

// admitted is a parsed, registered call waiting to execute.
type admitted struct {
	call       *hostedCall
	definition ToolDefinition
	name       string
	arguments  json.RawMessage
}

// prepare validates a call and registers it, synchronously, in the order the
// harness sent it. Everything after this point may take arbitrarily long; this
// part may not, because the connection cannot read the next frame until it
// returns, and one of those frames may be this call's cancellation.
//
// Registering before it waits for anything is also what makes a queued call
// real: it has to be cancellable, it has to count as unsettled, and it must not
// slip through a barrier that went up while it was waiting.
func (h *toolHost) prepare(request string, params json.RawMessage) (*admitted, map[string]any) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &in) != nil {
		return nil, h.refuse("", "malformed", "tool call could not be parsed; nothing was executed")
	}
	definition, hosted := h.tools[in.Name]
	if !hosted {
		return nil, h.refuse(in.Name, "unknown", "tool "+quoteName(in.Name)+" is not available in this session; nothing was executed")
	}
	if len(in.Arguments) == 0 || string(in.Arguments) == "null" {
		in.Arguments = json.RawMessage("{}")
	}
	// Handlers are written against an argument object. A bare string or array
	// that happens to be valid JSON is not one, and passing it through would
	// leave every handler to rediscover that.
	var arguments map[string]json.RawMessage
	if json.Unmarshal(in.Arguments, &arguments) != nil || arguments == nil {
		return nil, h.refuse(in.Name, "malformed", "tool arguments must be a JSON object; nothing was executed")
	}
	call, refusal := h.admit(request, in.Name)
	if refusal != nil {
		return nil, refusal
	}
	return &admitted{call: call, definition: definition, name: in.Name, arguments: in.Arguments}, nil
}

// execute runs an admitted call and decides whether it ended the work.
//
// Execution is serialized: a call waits for the previous one to finish. That is
// what makes "later" mean something. A closing tool then runs to completion like
// any other, and the channel latches only if it actually succeeded — a finish
// whose arguments were rejected has not finished anything, and neither has one
// whose handler refused it. Nothing is cancelled to make room for it, because a
// half-executed write or test is not a state worth reporting evidence about.
func (h *toolHost) execute(ready *admitted, turn string) map[string]any {
	defer h.retire(ready.call)
	if refusal := h.acquire(ready.call, ready.name); refusal != nil {
		return refusal
	}
	defer h.release()
	result, err := h.cfg.Handler.CallTool(ready.call.ctx, ToolCall{TurnID: turn, Name: ready.name, Arguments: ready.arguments})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return toolPayload("tool execution was cancelled; its effect is unknown and must be established from evidence", true)
		}
		return toolPayload(bound(err.Error(), 2048), true)
	}
	if !result.IsError && (ready.definition.Closing || result.Closes) {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
	}
	return toolPayload(bound(result.Content, h.cfg.MaxResultBytes), result.IsError)
}

// hostedCall is one admitted call, tracked from the moment it is accepted until
// it stops — including the time it spends queued.
type hostedCall struct {
	key        string
	ctx        context.Context
	cancel     context.CancelFunc
	generation uint64
}

// admit registers a call and decides whether it may proceed at all. It is the
// single barrier: everything that can refuse a call is here, and a call that
// gets past it is one this host will account for.
func (h *toolHost) admit(request, name string) (*hostedCall, map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	if refusal := h.barrierLocked(name); refusal != nil {
		h.mu.Unlock()
		cancel()
		return nil, refusal
	}
	call := &hostedCall{key: request, ctx: ctx, cancel: cancel, generation: h.generation}
	h.pending[request] = call
	h.trackSettlementLocked()
	h.mu.Unlock()
	// A stopping session ends everything it admitted.
	go func() {
		select {
		case <-h.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return call, nil
}

func (h *toolHost) retire(call *hostedCall) {
	h.mu.Lock()
	if existing, ok := h.pending[call.key]; ok && existing == call {
		delete(h.pending, call.key)
	}
	h.trackSettlementLocked()
	h.mu.Unlock()
	call.cancel()
}

// acquire waits for the serialization gate, then checks the barrier again. A
// call can be queued for a long time, and the thing it was waiting behind may
// have finished the work, paused the channel or stopped the session.
//
// Cancellation is checked on both sides of the gate. A select whose gate is free
// and whose call is already cancelled picks either case, so testing only the
// gate's readiness lets a withdrawn call run about half the time.
func (h *toolHost) acquire(call *hostedCall, name string) map[string]any {
	if call.ctx.Err() != nil {
		return h.refuse(name, "cancelled", "this call was withdrawn before it ran; nothing was executed")
	}
	select {
	case h.gate <- struct{}{}:
	case <-call.ctx.Done():
		return h.refuse(name, "cancelled", "this call was withdrawn before it ran; nothing was executed")
	case <-h.done:
		return h.refuse(name, "stopped", "this session has stopped; nothing was executed")
	}
	h.mu.Lock()
	refusal := h.barrierLocked(name)
	switch {
	case refusal != nil:
	case call.ctx.Err() != nil:
		// Withdrawn while it waited, or withdrawn before it ever waited and this
		// gate happened to be free.
		refusal = h.refuseLocked(name, "cancelled", "this call was withdrawn before it ran; nothing was executed")
	case call.generation != h.generation:
		// The channel was paused and reopened for different work while this call
		// waited. It belongs to the turn that is over.
		refusal = h.refuseLocked(name, "superseded", "this call belongs to work that has already been stopped; nothing was executed")
	}
	if refusal != nil {
		h.mu.Unlock()
		<-h.gate
		return refusal
	}
	h.running++
	h.trackSettlementLocked()
	h.mu.Unlock()
	return nil
}

func (h *toolHost) release() {
	h.mu.Lock()
	h.running--
	h.trackSettlementLocked()
	h.mu.Unlock()
	<-h.gate
}

// trackSettlementLocked keeps the settled channel closed exactly while nothing
// is outstanding, so a waiter is woken by the last call retiring rather than by
// a poll that might sample between two of them.
func (h *toolHost) trackSettlementLocked() {
	idle := h.settledLocked()
	select {
	case <-h.settled:
		if !idle {
			h.settled = make(chan struct{})
		}
	default:
		if idle {
			close(h.settled)
		}
	}
}

// settledLocked reports that no admitted call is queued or executing. Call it
// with h.mu held.
func (h *toolHost) settledLocked() bool { return h.running == 0 && len(h.pending) == 0 }

// barrierLocked is every reason a call may not run, in one place.
func (h *toolHost) barrierLocked(name string) map[string]any {
	switch {
	case h.stopped:
		return h.refuseLocked(name, "stopped", "this session has stopped; nothing was executed")
	case h.probing:
		return h.refuseLocked(name, "probing", "tool calls are refused during a capability check; nothing was executed")
	case h.closed:
		return h.refuseLocked(name, "channel_closed", "work has already been reported or handed over; nothing was executed")
	case h.paused:
		return h.refuseLocked(name, "paused", "this worker's tools are paused; nothing was executed")
	}
	return nil
}

func (h *toolHost) refuse(tool, reason, text string) map[string]any {
	if h.onRefusal != nil {
		h.onRefusal(tool, reason)
	}
	return toolPayload(text, true)
}

// refuseLocked is refuse from inside the host's own lock. The notification runs
// after the lock is released so a caller's observer cannot deadlock the channel.
func (h *toolHost) refuseLocked(tool, reason, text string) map[string]any {
	if notify := h.onRefusal; notify != nil {
		go notify(tool, reason)
	}
	return toolPayload(text, true)
}

func (h *toolHost) pause() {
	h.mu.Lock()
	h.paused = true
	calls := make([]*hostedCall, 0, len(h.pending))
	for _, call := range h.pending {
		calls = append(calls, call)
	}
	h.mu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

// closeAdmission stops new calls without disturbing the ones already admitted.
// A turn ending is a reason to stop accepting work, not a reason to abandon a
// write that is half-done. A nil host hosts nothing.
func (h *toolHost) closeAdmission() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.paused = true
	h.mu.Unlock()
}

// readyForWork refuses to authorize a turn while anything admitted is still
// outstanding. A nil host is the unrestricted case and hosts nothing.
func (h *toolHost) readyForWork() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settledLocked() {
		return nil
	}
	return ErrToolsUnsettled
}

// reopen takes only the host's own lock, so a caller already holding the
// session's may use it. A nil host is the unrestricted case and does nothing.
func (h *toolHost) reopen() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.stopped {
		return
	}
	h.paused = false
	h.generation++
}
