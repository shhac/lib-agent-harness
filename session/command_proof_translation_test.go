package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestCommandProofStepTranslation(t *testing.T) {
	for _, step := range []string{"", sandbox.ProofStepFixture, sandbox.ProofStepOutside, sandbox.ProofStepLaunch, sandbox.ProofStepJudgment, sandbox.ProofStepNetwork,
		sandbox.ProofStepNoOffMachineResolver, sandbox.ProofStepControlOnThisMachine, sandbox.ProofStepControlInvalidIP, sandbox.ProofStepControlInterfaceUnavailable, sandbox.ProofStepUDPUnanswered, sandbox.ProofStepTCPUnanswered, sandbox.ProofStepControlDeadline, sandbox.ProofStepPortClientUnavailable, sandbox.ProofStepIPv6ControlUnavailable, "untrusted raw text"} {
		t.Run(step, func(t *testing.T) {
			raw := &sandbox.ProofError{Code: sandbox.CapabilitySandboxNotEnforced, Step: step, ControlAddr: "192.0.2.53", ControlSource: "caller"}
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

func TestControlFailureFactsTranslation(t *testing.T) {
	previous := sandboxbridge.ProveWorkbench
	t.Cleanup(func() { sandboxbridge.ProveWorkbench = previous })
	for _, source := range []string{"caller", "configured", "systemd_upstream"} {
		for _, step := range []string{sandbox.ProofStepUDPUnanswered, sandbox.ProofStepTCPUnanswered, sandbox.ProofStepControlDeadline, sandbox.ProofStepNetwork} {
			raw := &sandbox.ProofError{Code: sandbox.CapabilitySandboxUnavailable, Step: step, ControlAddr: "192.0.2.53", ControlSource: source}
			translated := fromSandbox(raw, commandSandboxTranslation)
			a, _ := harness.ErrorFacts(raw)
			b, _ := harness.ErrorFacts(translated)
			if a != b || b.NetworkControlAddr != "192.0.2.53" || b.NetworkControlSource != source {
				t.Fatal("lost control facts", a, b)
			}
			sandboxbridge.ProveWorkbench = func(context.Context, any) (any, error) { return nil, raw }
			o := Options{Workbench: &Workbench{Commands: &Commands{Loopback: true, LoopbackPorts: []int{3000}}}}
			failed, _ := harness.ErrorFacts(proveWorkbench(t.Context(), o))
			if failed != a {
				t.Fatal("workbench proof lost control facts", failed, a)
			}
			if addr, source := (&Session{options: o}).NetworkControl(); addr != "" || source != "" {
				t.Fatal("failed proof published successful control")
			}
		}
	}
	raw := &sandbox.ProofError{Code: sandbox.CapabilitySandboxUnavailable, Step: sandbox.ProofStepNetwork, ControlAddr: "query-secret", ControlSource: "caller"}
	facts, _ := harness.ErrorFacts(fromSandbox(raw, commandSandboxTranslation))
	if facts.NetworkControlAddr != "" || facts.NetworkControlSource != "" {
		t.Fatal("untrusted DNS data escaped")
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
