//go:build darwin || linux

package sandbox

import (
	"context"
	"testing"
)

func TestProcessInspectionCancelledOuterProof(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, code := range []string{CapabilitySandboxNotEnforced, CapabilityProbeTimeout} {
		translated := executionProofError(ctx, "", &ProofError{Code: code, Step: ProofStepProcessInspection})
		if translated.Code != code || translated.Step != ProofStepProcessInspection {
			t.Fatalf("outer proof changed outcome: %v", translated)
		}
	}
}
