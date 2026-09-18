package session

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Process failure codes. Like every other code in this library they are fixed
// constants: no harness output is ever promoted into one.
const (
	ProcessExited      = "process_exited"
	ProcessSignalled   = "process_signalled"
	ProcessStartFailed = "process_start_failed"
)

// ProcessError reports that the harness process itself ended. It is a different
// fact from a protocol failure or a closed session, and reporting it separately
// is what keeps "the CLI died" from arriving as an unexplained transport error.
// An exit status does not establish whether a request was billed.
type ProcessError struct {
	Engine   string
	Code     string
	ExitCode *int
}

func (e *ProcessError) Error() string {
	out := e.Engine + " harness "
	switch e.Code {
	case ProcessSignalled:
		out += "was terminated by a signal"
	case ProcessStartFailed:
		out += "could not be started"
	default:
		out += "exited"
	}
	if e.ExitCode != nil && *e.ExitCode >= 0 {
		out += " with status " + strconv.Itoa(*e.ExitCode)
	}
	return out
}

// Unwrap keeps ErrTransport identity, so existing callers that classify a lost
// harness as a transport failure continue to work while gaining the detail.
func (e *ProcessError) Unwrap() error { return ErrTransport }

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
		out.ActiveTools = len(host.inflight)
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

// sanitize prepares captured harness output for a caller's private diagnostic
// record: control sequences removed, credential-shaped runs redacted, bounded.
// It is not a classification and must not be shown as one.
func sanitize(raw []byte, limit int) string {
	var out strings.Builder
	skipEscape := false
	for _, r := range string(raw) {
		if skipEscape {
			if unicode.IsLetter(r) {
				skipEscape = false
			}
			continue
		}
		switch {
		case r == 0x1b:
			skipEscape = true
		case r == '\n' || r == '\t':
			out.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			out.WriteRune(r)
		}
	}
	return bound(redactSecrets(strings.Join(strings.Fields(out.String()), " ")), limit)
}

// redactSecrets removes tokens that look like credentials. It is deliberately
// blunt: a long opaque run in diagnostic output is worth losing.
func redactSecrets(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'.,;:()[]{}`)
		if len(trimmed) >= 20 && opaque(trimmed) {
			fields[i] = strings.Replace(field, trimmed, "[redacted]", 1)
			continue
		}
		for _, prefix := range []string{"sk-", "sk_", "Bearer", "token=", "key=", "secret="} {
			if strings.HasPrefix(trimmed, prefix) && len(trimmed) > len(prefix) {
				fields[i] = "[redacted]"
				break
			}
		}
	}
	return strings.Join(fields, " ")
}

// opaque reports a run with no word structure: mixed classes, no separators and
// no vowel-bearing shape, which is what a credential looks like and ordinary
// prose does not.
func opaque(field string) bool {
	letters, digits, other := 0, 0, 0
	for _, r := range field {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			digits++
		default:
			other++
		}
	}
	if other > 2 || letters == 0 {
		return false
	}
	return digits > 0 || (letters > 24 && strings.ToLower(field) != field)
}
