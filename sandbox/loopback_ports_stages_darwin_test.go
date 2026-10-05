package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestSelectedPortProofPublication(t *testing.T) {
	previous := verified
	verified = &verificationCache{seen: map[string]bool{}}
	defer func() { verified = previous }()
	o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Loopback: true, LoopbackPorts: []int{3000}}
	baseRuns, portRuns := 0, 0
	base := func(_ context.Context, n Options, _ []string) error {
		baseRuns++
		if n.LoopbackPorts != nil || n.LoopbackControl != "" {
			t.Fatal("base proof inherited selected-port policy")
		}
		return nil
	}
	target := sandboxprobe.ControlTarget{Addr: "192.0.2.53", Source: "configured"}
	baseCtx, cancelBase := context.WithCancel(t.Context())
	_, baseErr := proveWorkbenchStages(baseCtx, o, func(context.Context, Options, []string) error { cancelBase(); return nil }, func(context.Context, Options, []string) (selectedPortEvidence, error) {
		t.Fatal("ports admitted after base cancellation")
		return selectedPortEvidence{}, nil
	})
	var baseFailure *ProofError
	if !errors.As(baseErr, &baseFailure) || baseFailure.Code != CapabilityProbeTimeout || len(verified.seen) != 0 {
		t.Fatal("cancelled base published evidence", baseErr)
	}
	for _, code := range []string{CapabilitySandboxUnavailable, CapabilitySandboxNotEnforced, CapabilityProbeTimeout} {
		_, err := proveWorkbenchStages(t.Context(), o, base, func(context.Context, Options, []string) (selectedPortEvidence, error) {
			return selectedPortEvidence{control: target}, &ProofError{Code: code, Step: proofStepSelectedPortNetwork}
		})
		var failure *ProofError
		if !errors.As(err, &failure) || failure.Code != code || failure.controlAddr != target.Addr || failure.controlSource != target.Source {
			t.Fatal(err)
		}
		if len(verified.seen) != 0 {
			t.Fatal("failed stage published evidence")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := proveWorkbenchStages(ctx, o, base, func(context.Context, Options, []string) (selectedPortEvidence, error) {
		cancel()
		return selectedPortEvidence{control: target}, nil
	})
	var failure *ProofError
	if !errors.As(err, &failure) || failure.Code != CapabilityProbeTimeout || failure.controlAddr != target.Addr || len(verified.seen) != 0 {
		t.Fatal("cancelled stage published or lost control", err)
	}
	ports := func(_ context.Context, n Options, _ []string) (selectedPortEvidence, error) {
		portRuns++
		return selectedPortEvidence{control: target}, nil
	}
	p, err := proveWorkbenchStages(t.Context(), o, base, ports)
	if err != nil || verified.network(p.key).control != target {
		t.Fatal("control evidence lost", err)
	}
	before := baseRuns
	if _, err := proveWorkbenchStages(t.Context(), o, base, ports); err != nil || portRuns != 1 || baseRuns != before {
		t.Fatal("matching proof not reused", err)
	}
	o.LoopbackPorts = []int{8080}
	if _, err := proveWorkbenchStages(t.Context(), o, base, ports); err != nil {
		t.Fatal(err)
	}
	o.LoopbackControl = "192.0.2.54"
	if _, err := proveWorkbenchStages(t.Context(), o, base, ports); err != nil {
		t.Fatal(err)
	}
	if portRuns != 3 {
		t.Fatal("incompatible proof reused")
	}
	_, err = proveWorkbenchStages(t.Context(), o, base, nil)
	var refused *RefusalError
	if !errors.As(err, &refused) || refused.Code != RefusedLoopbackPortsUnenforceable {
		t.Fatal("nil stage admitted", err)
	}
}

func TestSelectedPortDependencyFailuresPublishNothing(t *testing.T) {
	previous := verified
	verified = &verificationCache{seen: map[string]bool{}}
	defer func() { verified = previous }()
	o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Loopback: true, LoopbackPorts: []int{3000}}
	target := sandboxprobe.ControlTarget{Addr: "192.0.2.53", Source: "caller"}
	for _, step := range []string{sandboxprobe.ControlDeadline, sandboxprobe.NoOffMachineResolver, proofStepPortClientUnavailable, "cancel-control", "cancel-witness", "cancel-ready", "deadline-ready"} {
		t.Run(step, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			deps := selectedPortDeps{
				control: func(context.Context, string) (sandboxprobe.ControlTarget, string) {
					if step == "cancel-control" {
						cancel()
					}
					if step == sandboxprobe.ControlDeadline || step == sandboxprobe.NoOffMachineResolver {
						return target, step
					}
					return target, ""
				},
				witness: func(context.Context) (string, bool) {
					if step == "cancel-witness" {
						cancel()
					}
					return "192.0.2.1", true
				},
				ready: func(context.Context) error {
					if step == "deadline-ready" {
						return context.DeadlineExceeded
					}
					if step == "cancel-ready" {
						cancel()
					}
					return errors.New("missing helper")
				},
			}
			_, err := proveWorkbenchStages(ctx, o, func(context.Context, Options, []string) error { return nil }, func(ctx context.Context, n Options, system []string) (selectedPortEvidence, error) {
				return proveSelectedPortsWith(ctx, n, system, deps)
			})
			var failure *ProofError
			if !errors.As(err, &failure) || len(verified.seen) != 0 || failure.controlAddr != target.Addr {
				t.Fatal(err)
			}
			if strings.HasPrefix(step, "cancel-") || step == "deadline-ready" {
				if failure.Code != CapabilityProbeTimeout || failure.Step != proofStepSelectedPortNetwork {
					t.Fatal(err)
				}
			} else if failure.Code != CapabilitySandboxUnavailable || failure.Step != step {
				t.Fatal(err)
			}
		})
	}
}

func TestSelectedPortOwnerInterfaceTranscript(t *testing.T) {
	attempts := []interfaceAttempt{{Address: "192.0.2.100", Operation: "bind"}, {Address: "192.0.2.100", Operation: "udp-bind"}, {Address: "192.0.2.100", Operation: "udp"}}
	output := "interface-result:0:0\ninterface-result:1:0\ninterface-result:2:65\ninterface-canary-ran\n"
	_, err := judgeInterfaceAttempts(output, attempts, interfaceLoopbackOnly)
	var failure *ProofError
	if !errors.As(err, &failure) || failure.Code != CapabilitySandboxNotEnforced {
		t.Fatal("owner escape accepted", err)
	}
	observations, err := judgeInterfaceAttempts(output, attempts, interfaceAllLocal)
	if err != nil || len(observations) != 3 || observations[2].Errno != 65 {
		t.Fatal("escape observations lost", err)
	}
	for _, broken := range []string{strings.ReplaceAll(output, "interface-canary-ran\n", ""), output + "interface-result:0:0\n"} {
		if _, err := judgeInterfaceAttempts(broken, attempts, interfaceAllLocal); err == nil {
			t.Fatal("partial/duplicate observations accepted")
		}
	}
}

func selectedPortPrerequisite(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		testenv.SkipIfRefused(t, what, errors.Join(fs.ErrPermission, err))
	}
}

func TestSelectedPortCanaryStillEscapes(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	ctx, cancel := context.WithTimeout(t.Context(), sandboxProbeTimeout)
	defer cancel()
	o := commandSandboxOptions(t, false)
	o.Loopback = true
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	testenv.RequireExplicitLoopback(t, port)
	o.LoopbackPorts = []int{port}
	o.LoopbackControl = os.Getenv("AGENT_HARNESS_TEST_LOOPBACK_CONTROL")
	system, err := workbenchSystem(ctx)
	selectedPortPrerequisite(t, "developer tools", err)
	selectedPortPrerequisite(t, "selected-port socket client", portClientReady(ctx))
	control, reason := sandboxprobe.DiscoverAndProveControl(ctx, o.LoopbackControl)
	if reason != "" {
		selectedPortPrerequisite(t, "off-machine DNS control", fmt.Errorf("%s", reason))
	}
	o.LoopbackControl = control.Addr
	if _, ok := offMachineWitness(ctx); !ok {
		selectedPortPrerequisite(t, "off-machine witness", errors.New("unavailable"))
	}
	evidence, err := proveSelectedPorts(ctx, o, system)
	var failure *ProofError
	if !errors.As(err, &failure) || failure.Code != CapabilitySandboxNotEnforced || failure.Step != proofStepSelectedPortNetwork || len(evidence.observations) == 0 {
		t.Fatalf("re-audit selected-port Seatbelt confinement: err=%v observations=%+v", err, evidence.observations)
	}
	t.Logf("selected_port_network / sandbox_not_enforced; control=%s (%s); interface observations=%+v", failure.controlAddr, failure.controlSource, evidence.observations)
	for _, open := range []func() error{func() error { _, err := Prove(ctx, o); return err }, func() error { _, err := Open(ctx, o); return err }} {
		var refused *RefusalError
		if err := open(); !errors.As(err, &refused) || refused.Code != RefusedLoopbackPortsUnenforceable {
			t.Fatal("production admitted canary", err)
		}
	}
}

// The counter covers the production normalization boundary without launching
// Seatbelt, requiring tools or contacting any control.
func TestSelectedPortProductionNeverInvokesCanary(t *testing.T) {
	runs := 0
	_, err := proveOptions(t.Context(), Options{Loopback: true, LoopbackPorts: []int{3000}}, func(context.Context, Options) (Proof, error) { runs++; return Proof{}, nil })
	var refused *RefusalError
	if !errors.As(err, &refused) || refused.Code != RefusedLoopbackPortsUnenforceable || runs != 0 {
		t.Fatal("production canary reachable", err)
	}
}
