package catalog

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/apihttp"
)

// Phases say how far discovery got.
const (
	PhasePreflight = "preflight"
	PhaseProcess   = "process"
	PhaseTransport = "transport"
	PhaseResponse  = "response"
)

// Error is a failed discovery. Every field is a library constant or an
// allowlisted provider enum: no CLI output, provider prose, path or credential.
type Error struct {
	Engine harness.Engine
	Family harness.Family
	Cause  harness.Cause
	Phase  string
	// Code is fixed: a capability or preflight code (such as
	// "unsupported_engine" or "executable_not_found"), a process code
	// ("process_exited", "process_failed", "deadline_exceeded"), a response
	// code ("invalid_response", "request_failed", "missing_response",
	// "output_limit", "invalid_catalog", "catalog_limit", "page_limit",
	// "pagination_repeated", "credential_echoed"), or an HTTP code ("http_404").
	Code       string
	ExitCode   *int
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	detail := e.Code
	if e.Engine != "" {
		detail = string(e.Engine) + ", " + detail
	}
	return "model discovery failed (" + detail + "); saved model settings are unchanged"
}

// Unwrap lets errors.Is find a discovery timeout without retaining a raw error.
func (e *Error) Unwrap() error {
	if e.Cause == harness.CauseTimeout {
		return context.DeadlineExceeded
	}
	return nil
}

// Retryable reports an explicit transient rejection.
func (e *Error) Retryable() bool {
	return e.Cause == harness.CauseOverloaded || e.Cause == harness.CauseRateLimited || e.Cause == harness.CauseUnavailable
}

// HarnessFacts reports the failure in the vocabulary every operation shares.
func (e *Error) HarnessFacts() harness.Facts {
	return harness.Facts{
		Engine:     e.Engine,
		Operation:  harness.Models,
		Family:     e.Family,
		Cause:      e.Cause,
		Phase:      e.Phase,
		Code:       e.Code,
		ExitCode:   e.ExitCode,
		RetryAfter: e.RetryAfter,
		Retryable:  e.Retryable(),
	}
}

// refusal is a failure before any process or request. Capability codes will
// repeat for the same configuration; the rest can be fixed locally.
func refusal(engine harness.Engine, code string) *Error {
	if engine.Transport() == "" {
		engine = ""
	}
	family := harness.FailurePreflight
	switch code {
	case "unsupported_engine", "api_dialect_unsupported", "api_effort_parameter_unsupported":
		family = harness.FailureCapability
	}
	return &Error{Engine: engine, Family: family, Cause: harness.CauseUnknown, Phase: PhasePreflight, Code: code}
}

func preflightFailure(code string) *Error {
	return &Error{Family: harness.FailurePreflight, Cause: harness.CauseUnknown, Phase: PhasePreflight, Code: code}
}

func responseFailure(code string) *Error {
	return &Error{Family: harness.FailureRequest, Cause: harness.CauseUnknown, Phase: PhaseResponse, Code: code}
}

// classify turns whatever ended discovery into an *Error. Raw errors from a
// child or its pipes are reduced to fixed codes and dropped.
func classify(engine harness.Engine, bounded context.Context, err error) *Error {
	failure := classifyCause(bounded, err)
	failure.Engine = engine
	return failure
}

func classifyCause(bounded context.Context, err error) *Error {
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	var api *apihttp.Failure
	if errors.As(err, &api) {
		family := harness.FailureRequest
		if api.Phase == apihttp.PhasePreflight {
			family = harness.FailurePreflight
		}
		return &Error{Family: family, Cause: api.Cause, Phase: string(api.Phase), Code: api.Code, RetryAfter: api.RetryAfter}
	}
	if bounded.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Family: harness.FailureProcess, Cause: harness.CauseTimeout, Phase: PhaseProcess, Code: "deadline_exceeded"}
	}
	failure := &Error{Family: harness.FailureProcess, Cause: harness.CauseUnknown, Phase: PhaseProcess, Code: "process_failed"}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		failure.Code, failure.ExitCode = "process_exited", &code
	}
	return failure
}
