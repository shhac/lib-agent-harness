package session

import "errors"

// Structured facts about a failure, for callers that record, classify or display
// their own diagnostics.
//
// A caller with its own error vocabulary needs to know what went wrong without
// reading a message. Parsing Error() would make this library's prose part of its
// contract, and a caller that fell back to "unknown" whenever it did not
// recognize the concrete type would lose the very detail this library works to
// produce. So the facts are published directly, and every field is drawn from a
// fixed vocabulary: no harness output, provider prose, path or credential ever
// appears in one.

// Failure families. They say what kind of thing failed, so a caller can decide
// what to do about it without knowing which concrete error type carried it.
const (
	// FailureCapability: the installed harness could not be configured to the
	// restricted contract. Nothing was billed and no work was done.
	FailureCapability = "capability"
	// FailureProcess: the harness process itself started, ended or died. An exit
	// status does not establish whether a request was billed.
	FailureProcess = "process"
	// FailureTurn: the provider ended a turn in failure. Tools may already have
	// run, so this is not a safe thing to retry silently.
	FailureTurn = "turn"
)

// Facts is what a library error knows about itself.
type Facts struct {
	// Engine is the harness the failure came from, when one is known.
	Engine string `json:"engine,omitempty"`
	// Kind is one of the failure families above.
	Kind string `json:"kind,omitempty"`
	// Phase says how far things had got, for the failures where that is the
	// operative distinction. Empty when it does not apply.
	Phase string `json:"phase,omitempty"`
	// Code is the specific fixed code — a capability code, a process code, or a
	// provider's own enumerated terminal result.
	Code string `json:"code,omitempty"`
	// ExitCode is the harness's exit status where one was observed.
	ExitCode *int `json:"exit_code,omitempty"`
	// Retryable reports whether repeating the operation is safe on its own terms.
	// It is false for a failed turn even when the provider might succeed next
	// time, because a native turn may already have executed tools.
	Retryable bool `json:"retryable"`
}

// factual is implemented by this library's typed errors.
type factual interface{ HarnessFacts() Facts }

// ErrorFacts reports the structured facts an error carries, unwrapping to find
// them. The boolean says whether any were found; a caller that gets false is
// looking at something this library did not classify and should say so rather
// than inventing a code.
func ErrorFacts(err error) (Facts, bool) {
	var carrier factual
	if !errors.As(err, &carrier) {
		return Facts{}, false
	}
	return carrier.HarnessFacts(), true
}

func (e *CapabilityError) HarnessFacts() Facts {
	// A capability refusal is definitive: the same binary and configuration will
	// refuse again. It is fixed by changing the installation, not by waiting.
	return Facts{Engine: e.Engine, Kind: FailureCapability, Phase: e.Phase, Code: e.Code}
}

func (e *ProcessError) HarnessFacts() Facts {
	// Whether a lost process can be retried depends on what it had already done,
	// which this error does not know. Reporting it as retryable would be the
	// library deciding that for the caller.
	return Facts{Engine: e.Engine, Kind: FailureProcess, Code: e.Code, ExitCode: e.ExitCode}
}

func (e *TurnError) HarnessFacts() Facts {
	return Facts{Engine: e.Engine, Kind: FailureTurn, Code: e.Code}
}
