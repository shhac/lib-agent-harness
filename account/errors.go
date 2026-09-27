package account

import (
	"context"
	"errors"
	"strconv"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/session"
)

// Failure codes. Like every code in this library they are fixed constants: no
// provider text, path or credential ever enters one. A provider's own problem
// code (harness.Provider.Problem) is also reported as a Code.
const (
	// CodeUnsupportedEngine: harness.Support offers no account inspection for
	// the engine.
	CodeUnsupportedEngine = "unsupported_engine"
	// CodeNotInstalled: the CLI's executable was not found.
	CodeNotInstalled = "not_installed"
	// CodeMethodUnavailable: the installed CLI does not offer the account
	// method; an older or newer version may.
	CodeMethodUnavailable = "method_unavailable"
	// CodeInvalidResponse: the CLI answered in a shape the library cannot read.
	CodeInvalidResponse = "invalid_response"
	// CodeRejected: the CLI refused the request.
	CodeRejected = "rejected"
	// CodeTimedOut: the CLI did not answer in time.
	CodeTimedOut = "timed_out"
	// CodeTransport: the connection to the CLI failed.
	CodeTransport = "transport_failed"
	// CodeProcessExited, CodeProcessSignalled and CodeProcessStartFailed: the
	// CLI process itself ended or could not start.
	CodeProcessExited      = session.ProcessExited
	CodeProcessSignalled   = session.ProcessSignalled
	CodeProcessStartFailed = session.ProcessStartFailed
)

// Error is a failed inspection. It unwraps to the underlying library error,
// so a session sentinel such as session.ErrUnsupported still matches.
type Error struct {
	Engine   harness.Engine
	Code     string
	Family   harness.Family
	Cause    harness.Cause
	ExitCode *int
	err      error
}

func (e *Error) Error() string {
	message := map[string]string{
		CodeUnsupportedEngine:  "account inspection is not offered for this engine",
		CodeNotInstalled:       "the CLI is not installed",
		CodeMethodUnavailable:  "the installed CLI does not offer account inspection",
		CodeInvalidResponse:    "the CLI answered in an unrecognized shape",
		CodeRejected:           "the CLI refused the account request",
		CodeTimedOut:           "the CLI did not answer in time",
		CodeTransport:          "the connection to the CLI failed",
		CodeProcessExited:      "the CLI exited",
		CodeProcessSignalled:   "the CLI was terminated by a signal",
		CodeProcessStartFailed: "the CLI could not be started",
	}[e.Code]
	if message == "" {
		message = "the provider is not a well-formed CLI provider (" + e.Code + ")"
	}
	if e.ExitCode != nil && *e.ExitCode >= 0 {
		message += " with status " + strconv.Itoa(*e.ExitCode)
	}
	return string(e.Engine) + " account inspection: " + message
}

func (e *Error) Unwrap() error { return e.err }

// HarnessFacts reports the failure. Inspection only reads, so repeating it is
// safe whenever the failure could pass: a timeout, a lost connection or a
// process that ended. A refusal or an unreadable answer would recur.
func (e *Error) HarnessFacts() harness.Facts {
	retryable := e.Code == CodeTimedOut || e.Family == harness.FailureProcess
	return harness.Facts{Engine: e.Engine, Operation: harness.Account, Family: e.Family, Cause: e.Cause, Code: e.Code, ExitCode: e.ExitCode, Retryable: retryable}
}

// fromSession restates a session inspection failure as an account one. A
// joined error (the account and quota reads fail separately) is classified by
// its most specific part.
func fromSession(engine harness.Engine, err error) *Error {
	out := &Error{Engine: engine, err: err}
	var process *session.ProcessError
	var unsupported *session.UnsupportedError
	switch {
	case errors.As(err, &process):
		out.Family, out.Code, out.ExitCode = harness.FailureProcess, process.Code, process.ExitCode
	case errors.As(err, &unsupported):
		out.Family, out.Code = harness.FailurePreflight, unsupported.Code
		if facts, ok := harness.ErrorFacts(unsupported); ok {
			out.Family = facts.Family
		}
	case errors.Is(err, session.ErrUnsupported):
		out.Family, out.Code = harness.FailureCapability, CodeMethodUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		out.Family, out.Code, out.Cause = harness.FailureRequest, CodeTimedOut, harness.CauseTimeout
	case errors.Is(err, session.ErrRejected):
		out.Family, out.Code, out.Cause = harness.FailureRequest, CodeRejected, harness.CauseUnknown
	case errors.Is(err, session.ErrProtocol), errors.Is(err, session.ErrOutputLimit):
		out.Family, out.Code = harness.FailureRequest, CodeInvalidResponse
	default:
		out.Family, out.Code = harness.FailureProcess, CodeTransport
	}
	return out
}
