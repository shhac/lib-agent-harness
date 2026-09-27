package session

import harness "github.com/shhac/lib-agent-harness"

// Each typed failure publishes its facts in the shared harness vocabulary, so
// a caller classifies a session failure exactly as it would any other mode's,
// with harness.ErrorFacts. Every field is drawn from a fixed vocabulary: no
// harness output, provider prose, path or credential ever appears in one.

func (e *CapabilityError) HarnessFacts() harness.Facts {
	// A capability refusal is definitive: the same binary and configuration will
	// refuse again. It is fixed by changing the installation, not by waiting.
	return harness.Facts{Engine: e.Engine, Operation: harness.Session, Family: harness.FailureCapability, Phase: e.Phase, Code: e.Code}
}

func (e *ProcessError) HarnessFacts() harness.Facts {
	// Whether a lost process can be retried depends on what it had already done,
	// which this error does not know. Reporting it as retryable would be the
	// library deciding that for the caller.
	return harness.Facts{Engine: e.Engine, Operation: harness.Session, Family: harness.FailureProcess, Code: e.Code, ExitCode: e.ExitCode}
}

func (e *TurnError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: e.Engine, Operation: harness.Session, Family: harness.FailureTurn, Code: e.Code}
}

func (e *UnsupportedError) HarnessFacts() harness.Facts {
	// Refused before it was attempted, so nothing ran; but the same
	// configuration refuses again, so repeating it is not a remedy.
	return harness.Facts{Engine: e.Engine, Operation: harness.Session, Family: refusalFamily(e.Code), Code: e.Code}
}
