// Package session controls persistent, native CLI agent sessions. Native tools
// are available according to the explicitly configured provider policy. This is
// not the tools-disabled completion API.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
type Capabilities struct{ Start, Resume, Interrupt, Steer, ReplaceInstructions, AppendInstructions Capability }

// CapabilitiesFor describes availability before contacting an installed CLI.
// Strategy alone is not evidence that a particular installed version supports it.
func CapabilitiesFor(e Engine) Capabilities {
	u := Capability{Unknown, "not verified against the installed harness"}
	if e != Codex && e != Claude {
		u = Capability{Unsupported, "unrecognized harness"}
	}
	return Capabilities{u, u, u, u, u, u}
}

var (
	ErrUnsupported        = errors.New("harness operation unsupported")
	ErrClosed             = errors.New("harness session closed")
	ErrBusy               = errors.New("harness turn already active")
	ErrStaleTurn          = errors.New("harness turn does not match expected active turn")
	ErrIncompatibleResume = errors.New("harness resume configuration does not match reference")
	ErrBackpressure       = errors.New("harness event buffer exhausted; consume Events while the turn runs")
	ErrProtocol           = errors.New("invalid harness protocol response")
	// ErrRejected is a definitive server refusal. No retry is automatic, but
	// the session remains usable. Provider error text is intentionally omitted.
	ErrRejected    = errors.New("harness rejected operation")
	ErrOutputLimit = errors.New("harness output exceeded its bounded frame or text limit")
	ErrTransport   = errors.New("harness transport failed")
	ErrTurnFailed  = errors.New("harness turn failed")
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
	// EventBuffer defaults to 256; MaxTextBytes defaults to 1 MiB per turn.
	EventBuffer, MaxTextBytes int
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
	Kind   string `json:"kind"` // text_delta, text, tool_started, tool_completed, status, usage
	TurnID string `json:"turn_id"`
	ItemID string `json:"item_id,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Status string `json:"status,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
}

// Usage is per-turn. Input excludes CacheRead; Reasoning is a subset of Output.
// Known distinguishes unavailable accounting from zero usage.
type Usage struct {
	Known                                           bool
	Input, Output, CacheRead, CacheWrite, Reasoning int64
}
type Result struct {
	// NativeError preserves a native failure indication, including Claude's
	// error_during_execution result after a requested interruption. Correlation
	// proves the turn ended, not that interruption was the sole failure cause.
	NativeError          bool
	TurnID, Text, Status string
	Usage                Usage
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
	pending            []map[string]json.RawMessage
	pendingBytes       int
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
		return t.result, t.err
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
