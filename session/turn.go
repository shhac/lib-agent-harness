package session

import (
	"context"
	"encoding/json"
	"sync"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/claudeproto"
	"github.com/shhac/lib-agent-harness/internal/tokens"
)

// Usage is what a provider reported, in the shared harness shape: Input counts
// every prompt token, cached or not, and the cache figures are parts of it.
//
// Final separates a turn's own accounting from an observation of one model
// response inside it. A turn makes many requests, so a caller that wants to
// react while work is still running has to read the non-final ones — and a
// caller that wants the turn's accounting must not sum them.
type Usage struct {
	harness.Usage
	Final bool `json:"final"`
}

// add accumulates one response's figures. Anything that would not stay
// representable makes the accumulation unknown rather than wrapping into a
// number that looks measured. The cache split stays known only while every
// response reported it.
func (u Usage) add(next Usage) Usage {
	if !next.Known {
		return u
	}
	return Usage{Usage: tokens.Add(u.Usage, next.Usage)}
}

type Result struct {
	// NativeError preserves a native failure indication, including Claude's
	// error_during_execution result after a requested interruption. Correlation
	// proves the turn ended, not that interruption was the sole failure cause.
	NativeError          bool
	TurnID, Text, Status string
	Usage                Usage
	// Observed accumulates each model response's reported usage during this
	// turn. It survives a failed or interrupted turn, where the provider's own
	// terminal accounting is often absent or zero despite work having happened.
	// It is evidence of what was seen, not a measurement of the turn: a caller
	// with a budget should still treat an unknown Usage as an unknown call.
	Observed Usage
	Context  ContextSnapshot
}

type Turn struct {
	mu                 sync.Mutex
	id                 string
	events             chan Event
	done               chan struct{}
	result             Result
	err                error
	finished           bool
	textItem           string
	lastCodexTotal     codexUsage
	interruptRequested bool
	starting           bool
	compacting         bool
	awaitingCompactID  bool
	pending            []map[string]json.RawMessage
	pendingBytes       int
	// acpMessage counts an ACP turn's messages: Grok's completed model
	// responses, or the tool calls Command Code and fx started. It names the
	// message the next streamed text belongs to.
	acpMessage int
	// antigravityTools tracks which Antigravity tool step IDs have emitted
	// tool_started and tool_completed in this turn.
	antigravityTools map[string]bool
	// antigravityUsageGap records a response in this turn that reported no
	// usable counts, which leaves the turn's settled usage unknown.
	antigravityUsageGap bool
	// closeTools shuts the caller's tool channel when this turn ends.
	closeTools func()
	// claudeRefusal is why Claude refused this turn's latest request, kept for
	// the errored result that follows.
	claudeRefusal claudeproto.Refusal
}

func (t *Turn) ID() string { t.mu.Lock(); defer t.mu.Unlock(); return t.id }

func (t *Turn) Events() <-chan Event { return t.events }

// ended reports that this turn's stream is closed, so anything emitted to it now
// would be dropped rather than observed.
func (t *Turn) ended() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.finished }

// Wait does not drain Events; callers should consume the bounded event stream
// concurrently. Context cancellation here only stops waiting.
func (t *Turn) Wait(ctx context.Context) (Result, error) {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		result := t.result
		result.Context = cloneContext(result.Context)
		return result, t.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (t *Turn) finish(status string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	t.result.Status = status
	t.result.TurnID = t.id
	t.err = err
	// Tool admission closes with the turn, not when the caller gets around to
	// noticing it has ended. Both installed harnesses have been observed sending
	// a tool call after their terminal result, and there is always a gap between
	// a turn ending and a caller reacting to it — so a caller that closed the
	// channel on the terminal event would still be racing. Anything already
	// running is left alone: it is cancellation that stops work, and a turn
	// ending is not a reason to abandon a write half-done.
	if t.closeTools != nil {
		t.closeTools()
	}
	close(t.events)
	close(t.done)
}

func (t *Turn) emit(e Event) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	e.TurnID = t.id
	select {
	case t.events <- e:
		return nil
	default:
		return ErrBackpressure
	}
}
