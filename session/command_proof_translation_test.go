package session

import (
	"errors"
	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
	"strings"
	"testing"
)

func TestCommandProofStepTranslation(t *testing.T) {
	for _, step := range []string{"", sandbox.ProofStepFixture, sandbox.ProofStepOutside, sandbox.ProofStepLaunch, sandbox.ProofStepJudgment, "untrusted raw text"} {
		t.Run(step, func(t *testing.T) {
			raw := &sandbox.ProofError{Code: sandbox.CapabilitySandboxNotEnforced, Step: step}
			err := fromSandbox(raw, commandSandboxTranslation)
			var capability *CapabilityError
			if !errors.As(err, &capability) {
				t.Fatal(err)
			}
			want, _ := harness.ErrorFacts(raw)
			got, _ := harness.ErrorFacts(err)
			if capability.ProofStep != want.ProofStep || got != want || err.Error() != raw.Error() {
				t.Fatalf("translation drift: %+v / %+v; %v / %v", got, want, err, raw)
			}
			if step == "" || step == "untrusted raw text" {
				if strings.Contains(err.Error(), "proof step:") || strings.Contains(err.Error(), "reads, execution") {
					t.Fatal(err)
				}
			} else if !strings.Contains(err.Error(), "; proof step: "+step) {
				t.Fatal(err)
			}
		})
	}
}

func TestDeprecatedStartPrelaunchNilHandle(t *testing.T) {
	_, wrapper := fakeWrappedCommands(t, CommandStartFailed)
	defer wrapper.Close()
	for _, request := range []CommandRequest{{}, {Command: "true", Timeout: -1}} {
		h, err := wrapper.Start(t.Context(), request)
		if h != nil || err == nil {
			t.Fatalf("pre-launch: %v %v", h, err)
		}
	}
	wrapper.Close()
	h, err := wrapper.Start(t.Context(), CommandRequest{Command: "true"})
	if h != nil || err == nil {
		t.Fatalf("closed: %v %v", h, err)
	}
}
