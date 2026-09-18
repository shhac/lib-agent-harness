//go:build !windows

package session

import (
	"errors"
	"testing"
)

// The facts a caller records come from the error itself. Reading them out of a
// message would make this library's prose part of its contract, and falling back
// to "unknown" for anything unrecognized would throw away the classification
// this library works to produce.
func TestTypedFailuresPublishTheirFacts(t *testing.T) {
	status := 3
	for _, tc := range []struct {
		name string
		err  error
		want Facts
	}{
		{"turn", &TurnError{Engine: "claude", Code: "authentication_failed"},
			Facts{Engine: "claude", Kind: FailureTurn, Code: "authentication_failed"}},
		{"process", &ProcessError{Engine: "codex", Code: ProcessExited, ExitCode: &status},
			Facts{Engine: "codex", Kind: FailureProcess, Code: ProcessExited, ExitCode: &status}},
		{"capability", &CapabilityError{Engine: "codex", Code: CapabilityNativeToolsPresent, Phase: BeforeLaunch},
			Facts{Engine: "codex", Kind: FailureCapability, Code: CapabilityNativeToolsPresent, Phase: BeforeLaunch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped, because that is how a caller receives it.
			got, ok := ErrorFacts(errors.Join(errors.New("context"), tc.err))
			if !ok {
				t.Fatal("a typed failure carried no facts")
			}
			if got.Engine != tc.want.Engine || got.Kind != tc.want.Kind || got.Code != tc.want.Code || got.Phase != tc.want.Phase {
				t.Fatalf("facts disagree: %+v", got)
			}
			if (got.ExitCode == nil) != (tc.want.ExitCode == nil) {
				t.Fatalf("exit status lost: %+v", got)
			}
			if got.Retryable {
				t.Error("a failure that may have executed tools was reported as safe to repeat")
			}
		})
	}
	if _, ok := ErrorFacts(errors.New("something else")); ok {
		t.Error("an unclassified error was reported as carrying facts")
	}
}
