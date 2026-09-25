package session

import (
	"errors"
	"time"
)

// HealthState describes what is currently observable about a session. It says
// nothing about whether the work is going well, and nothing about whether it is
// finished: only an explicit tool call ends work, and only evidence accepts it.
type HealthState string

const (
	// Running: the harness is alive and a turn is in progress.
	Running HealthState = "running"
	// Active: output or a tool call is happening right now.
	Active HealthState = "active"
	// Quiet: alive, but nothing observed recently. This is unknown, not stuck.
	Quiet HealthState = "quiet"
	// Idle: alive with no turn in progress.
	Idle HealthState = "idle"
	// Exited: the harness process ended.
	Exited HealthState = "exited"
	// Failed: the provider or protocol reported an explicit failure.
	Failed HealthState = "failed"
)

// Health is a snapshot. LastEventAt is zero when nothing has been observed yet,
// which is not evidence of silence in a session that has only just started.
type Health struct {
	State       HealthState `json:"state"`
	LastEventAt time.Time   `json:"last_event_at,omitzero"`
	ActiveTools int         `json:"active_tools"`
	TurnID      string      `json:"turn_id,omitempty"`
	// Reason is set for Exited and Failed, from the fixed code vocabulary.
	Reason string `json:"reason,omitempty"`
}

// Health reports the session's currently observable state. It performs no I/O
// and never asks the harness anything: a health check that woke a quiet session
// would be changing what it measures.
func (s *Session) Health() Health {
	s.mu.Lock()
	closed, failure, last, t := s.closed, s.failure, s.lastEvent, s.active
	quiet := s.options.QuietAfter
	host := s.tools
	s.mu.Unlock()
	if quiet <= 0 {
		quiet = 2 * time.Minute
	}
	out := Health{State: Running, LastEventAt: last}
	if host != nil {
		host.mu.Lock()
		out.ActiveTools = host.running
		host.mu.Unlock()
	}
	if t != nil {
		out.TurnID = t.ID()
	}
	switch {
	case failure != nil:
		var exit *ProcessError
		if errors.As(failure, &exit) {
			out.State, out.Reason = Exited, exit.Code
			return out
		}
		out.State, out.Reason = Failed, failureReason(failure)
		return out
	case closed:
		out.State, out.Reason = Exited, ProcessExited
		return out
	}
	active := false
	if t != nil {
		select {
		case <-t.done:
		default:
			active = true
		}
	}
	if !active {
		out.State = Idle
		return out
	}
	if out.ActiveTools > 0 || (!last.IsZero() && time.Since(last) < quiet) {
		out.State = Active
		return out
	}
	if !last.IsZero() {
		out.State = Quiet
	}
	return out
}

func failureReason(err error) string {
	for _, known := range []struct {
		error
		code string
	}{
		{ErrOutputLimit, "output_limit"}, {ErrProtocol, "protocol"},
		{ErrBackpressure, "event_backpressure"}, {ErrTurnFailed, "turn_failed"},
		{ErrRejected, "rejected"}, {ErrUnsupported, "unsupported"},
		{ErrTransport, "transport"}, {ErrClosed, "closed"},
	} {
		if errors.Is(err, known.error) {
			return known.code
		}
	}
	return "unknown"
}
