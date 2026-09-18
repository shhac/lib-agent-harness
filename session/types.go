// Package session controls persistent, native CLI agent sessions. Native tools
// are available according to the explicitly configured provider policy. This is
// not the tools-disabled completion API.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// MaxFrameBytes bounds one native JSON-line frame, including discarded tool
// payloads. Oversized frames stop the session with ErrOutputLimit.
const MaxFrameBytes = 16 << 20

type Engine string

const (
	Codex  Engine = "codex"
	Claude Engine = "claude"
)

type Availability string

const (
	Native      Availability = "native"
	Composed    Availability = "composed"
	Unsupported Availability = "unsupported"
	Unknown     Availability = "unknown"
)

type Capability struct {
	Availability Availability `json:"availability"`
	Reason       string       `json:"reason,omitempty"`
}
type Capabilities struct {
	Start, Resume, Interrupt, Steer, ReplaceInstructions, AppendInstructions Capability
	// Telemetry capabilities become Native only after a successful response or
	// event. Older CLI versions may reject optional inspection methods.
	Account, Quota, Context, Compact Capability
	// RestrictTools reports whether the installed harness was observed running
	// with exactly the configured tool surface. Unknown means it was not checked
	// on this path, never that it was checked and found acceptable.
	RestrictTools Capability
}

// CapabilitiesFor describes availability before contacting an installed CLI.
// Strategy alone is not evidence that a particular installed version supports it.
func CapabilitiesFor(e Engine) Capabilities {
	u := Capability{Unknown, "not verified against the installed harness"}
	if e != Codex && e != Claude {
		u = Capability{Unsupported, "unrecognized harness"}
	}
	compact := u
	if e == Claude {
		compact = Capability{Unsupported, "Claude exposes no verified manual compaction control protocol"}
	}
	return Capabilities{Compact: compact, Start: u, Resume: u, Interrupt: u, Steer: u, ReplaceInstructions: u, AppendInstructions: u, Account: u, Quota: u, Context: u}
}

var (
	ErrUnsupported = errors.New("harness operation unsupported")
	ErrClosed      = errors.New("harness session closed")
	ErrBusy        = errors.New("harness turn already active")
	ErrStaleTurn   = errors.New("harness turn does not match expected active turn")
	// ErrToolsUnsettled reports an attempt to start work while a tool call from
	// the previous turn is still outstanding. Cancelling a call asks its handler
	// to stop; until the handler returns, what it did is unknown, and authorizing
	// another turn on top of it would be building on a workspace still in motion.
	ErrToolsUnsettled     = errors.New("harness tool calls from the previous turn have not settled")
	ErrIncompatibleResume = errors.New("harness resume configuration does not match reference")
	ErrBackpressure       = errors.New("harness event buffer exhausted; consume Events while the turn runs")
	ErrProtocol           = errors.New("invalid harness protocol response")
	// ErrRejected is a definitive server refusal. No retry is automatic, but
	// the session remains usable. Provider error text is intentionally omitted.
	ErrRejected    = errors.New("harness rejected operation")
	ErrOutputLimit = errors.New("harness output exceeded its bounded frame or text limit")
	ErrTransport   = errors.New("harness transport failed")
	ErrTurnFailed  = errors.New("harness turn failed")
	// ErrLeaseHeld reports that another process already holds this session's
	// assignment lease. Two processes driving one assignment would spend the
	// same account twice; the second one stops instead.
	ErrLeaseHeld = errors.New("another process holds this session's assignment lease")
)

type UnsupportedError struct {
	Operation  string
	Capability Capability
}

func (e *UnsupportedError) Error() string { return e.Operation + ": " + e.Capability.Reason }
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

type InstructionMode string

const (
	Replace InstructionMode = "replace"
	Append  InstructionMode = "append"
)

// Instructions apply at session creation and must remain identical on resume.
// Replace replaces the native base prompt; Append uses provider-supported
// additional instructions. Neither suppresses project files or managed policy.
type Instructions struct {
	Mode InstructionMode `json:"mode,omitempty"`
	Text string          `json:"text,omitempty"`
}

// Policy uses explicit native provider settings. Empty values resolve to
// read-only/never for Codex and dontAsk for Claude. Unhandled requests from the
// CLI are denied. These are execution settings, not a tools-disabled guarantee.
type Policy struct {
	CodexSandbox     string `json:"codex_sandbox,omitempty"`
	CodexApproval    string `json:"codex_approval,omitempty"`
	ClaudePermission string `json:"claude_permission,omitempty"`
	// ClaudeTools nil preserves native tools; a non-nil empty slice disables
	// native tools. This does not disable installed hooks or MCP configuration.
	ClaudeTools []string `json:"claude_tools,omitempty"`
}
type Options struct {
	Engine                               Engine
	Binary, Home, WorkDir, Model, Effort string
	// AccountIdentity is a caller-owned, non-secret label. Credentials are never
	// read or copied. Change this label when deliberately changing an account.
	AccountIdentity string
	Instructions    Instructions
	Policy          Policy
	// RuntimeHome is the durable private home a restricted session runs in. The
	// library owns its configuration and shares only the login from Home, so a
	// worker gets the operator's account without the rest of their setup. It must
	// be separate from Home, must persist for the assignment's life — the native
	// conversation lives in it — and belongs in private application state.
	RuntimeHome string
	// Restriction opts this session into the restricted worker contract: the
	// harness's own tools are removed and replaced by the caller's, verified
	// against the installed CLI before a credentialed process starts. Leaving it
	// nil keeps the ordinary native contract exactly as it was.
	Restriction *Restriction
	// OnDiagnostic receives bounded, sanitized detail about a failure, once, for
	// the caller's own private records. It is deliberately not part of any error
	// value: like provider text, captured harness output does not belong in a
	// message that might reach a log or a user interface by default.
	OnDiagnostic func(Diagnostic)
	// QuietAfter is how long without an event makes a live session Quiet rather
	// than Running. Quiet is unknown, never stuck. Defaults to two minutes.
	QuietAfter time.Duration
	// EventBuffer defaults to 256; MaxTextBytes defaults to 1 MiB per turn.
	EventBuffer, MaxTextBytes int
}

// Diagnostic carries what a failure looked like locally. Detail is a bounded,
// control-stripped tail of the harness's own standard error with credential-
// shaped runs removed; it is evidence for an operator, not a classification.
type Diagnostic struct {
	Engine string    `json:"engine"`
	Stage  string    `json:"stage"`
	Code   string    `json:"code"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// Ref is safe to persist as private application state. It contains local paths
// and an opaque configuration digest, never instructions or credentials. It
// binds the selected home/account label, not the identity of a refreshed login.
type Ref struct {
	Engine          Engine `json:"engine"`
	ID              string `json:"id"`
	Home            string `json:"home"`
	WorkDir         string `json:"work_dir"`
	AccountIdentity string `json:"account_identity,omitempty"`
	ConfigHash      string `json:"config_hash"`
}
type Input struct{ Text string }
type SteerOptions struct{ RequireNative bool }
type SteerResult struct {
	Strategy Availability
	Turn     *Turn
}
type Event struct {
	Kind    string           `json:"kind"` // text_delta, text, tool_started, tool_completed, status, usage, context, quota, account
	TurnID  string           `json:"turn_id"`
	ItemID  string           `json:"item_id,omitempty"`
	Text    string           `json:"text,omitempty"`
	Tool    string           `json:"tool,omitempty"`
	Status  string           `json:"status,omitempty"`
	Usage   *Usage           `json:"usage,omitempty"`
	Context *ContextSnapshot `json:"context,omitempty"`
	Quota   *QuotaSnapshot   `json:"quota,omitempty"`
	Account *AccountSnapshot `json:"account,omitempty"`
}

// Usage is what a provider reported. Input excludes CacheRead; Reasoning is a
// subset of Output. Known distinguishes unavailable accounting from zero usage.
//
// Final separates a turn's own accounting from an observation of one model
// response inside it. A turn makes many requests, so a caller that wants to
// react while work is still running has to read the non-final ones — and a
// caller that wants the turn's accounting must not sum them.
type Usage struct {
	Known                                           bool
	Final                                           bool
	Input, Output, CacheRead, CacheWrite, Reasoning int64
}

// add accumulates one response's figures. Anything that would not stay
// representable makes the accumulation unknown rather than wrapping into a
// number that looks measured.
func (u Usage) add(next Usage) Usage {
	if !next.Known {
		return u
	}
	columns := [][2]int64{{u.Input, next.Input}, {u.Output, next.Output}, {u.CacheRead, next.CacheRead}, {u.CacheWrite, next.CacheWrite}, {u.Reasoning, next.Reasoning}}
	for _, c := range columns {
		if c[1] < 0 || c[0] > maxInt64-c[1] {
			return Usage{}
		}
	}
	return Usage{Known: true, Input: u.Input + next.Input, Output: u.Output + next.Output, CacheRead: u.CacheRead + next.CacheRead, CacheWrite: u.CacheWrite + next.CacheWrite, Reasoning: u.Reasoning + next.Reasoning}
}

const maxInt64 = int64(^uint64(0) >> 1)

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
	// closeTools shuts the caller's tool channel when this turn ends.
	closeTools func()
}

func (t *Turn) ID() string           { t.mu.Lock(); defer t.mu.Unlock(); return t.id }
func (t *Turn) Events() <-chan Event { return t.events }

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
