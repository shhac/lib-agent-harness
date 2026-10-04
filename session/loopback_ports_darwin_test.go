package session

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/sandbox"
)

// Exercise actual session recovery/admission with a synthetic sandbox transport;
// file tools stay disabled and no real runtime or network control is invoked.
func TestWorkbenchSelectedPortsRecoveryAndFreeze(t *testing.T) {
	previousProbe, previousRunner, previousNetwork := sandboxbridge.ProveWorkbench, newWorkbenchRunner, sandboxbridge.NormalizeNetwork
	previousWorkbench := sandboxbridge.NormalizeWorkbench
	defer func() {
		sandboxbridge.ProveWorkbench = previousProbe
		newWorkbenchRunner = previousRunner
		sandboxbridge.NormalizeNetwork = previousNetwork
		sandboxbridge.NormalizeWorkbench = previousWorkbench
	}()
	// This lifecycle fixture owns a synthetic off-machine inventory. It must
	// not depend on host interface inspection, which the daemon can refuse.
	// Production normalization and separate control-policy tests keep the guard.
	sandboxbridge.NormalizeNetwork = func(v any) (any, error) {
		o := v.(sandbox.Options)
		control := o.LoopbackControl
		if control != "" && control != "192.0.2.53" {
			t.Fatalf("unexpected synthetic control %q", control)
		}
		o.LoopbackControl = ""
		n, err := previousNetwork(o)
		if err != nil {
			return n, err
		}
		frozen := n.(sandbox.Options)
		frozen.LoopbackControl = control
		return frozen, nil
	}
	// The final command normalization shares the same inventory guard; keep
	// this fixture synthetic at both internal entry points.
	sandboxbridge.NormalizeWorkbench = func(v any) (any, error) {
		o := v.(sandbox.Options)
		control := o.LoopbackControl
		o.LoopbackControl = ""
		n, err := previousWorkbench(o)
		if err != nil {
			return n, err
		}
		frozen := n.(sandbox.Options)
		frozen.LoopbackControl = control
		return frozen, nil
	}
	probes := 0
	sandboxbridge.ProveWorkbench = func(_ context.Context, v any) (any, error) {
		o := v.(sandbox.Options)
		if !slices.Equal(o.LoopbackPorts, []int{3000, 8080}) {
			t.Errorf("unfrozen probe options: %v", o.LoopbackPorts)
		}
		probes++
		return sandbox.Proof{}, nil
	}
	newWorkbenchRunner = func(o sandbox.Options, _ sandbox.Proof, _ string, _ int) (*sandbox.Runner, error) {
		if !slices.Equal(o.LoopbackPorts, []int{3000, 8080}) {
			t.Errorf("launch policy drift: %v", o.LoopbackPorts)
		}
		r := &sandbox.Runner{}
		*sandboxhook.RunnerAccess(r).Close = func() error { return nil }
		return r, nil
	}
	o := workbenchOptions(t, nopHandler())
	o.complete = (&scriptedModel{}).complete
	ports := []int{8080, 3000, 8080}
	o.Workbench = &Workbench{Commands: &Commands{Loopback: true, LoopbackPorts: ports}}
	s := startAPI(t, o)
	ref := s.Ref()
	ports[0] = 9000
	if !slices.Equal(s.options.Workbench.Commands.LoopbackPorts, []int{3000, 8080}) {
		t.Fatal("caller mutation changed session policy")
	}
	closeAPI(t, s)
	o.Workbench.Commands.LoopbackPorts = []int{3000, 8080}
	// Interrupted command records retain their unknown-outcome semantics.
	store, _, err := openTranscript(o.RuntimeHome, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []record{{Type: recordTurnStart, Turn: "interrupted"}, {Type: recordToolCall, Turn: "interrupted", Call: "command", Tool: workbenchRunCommand}} {
		if err := store.append(r); err != nil {
			t.Fatal(err)
		}
	}
	store.close()
	for _, change := range []string{"same", "control"} {
		if change == "control" {
			o.Workbench.Commands.LoopbackControl = "192.0.2.53"
		}
		resumed, err := Resume(context.Background(), o, ref)
		if err != nil {
			t.Fatal(err)
		}
		if change == "same" && resumed.Recovered().TurnID != "interrupted" {
			t.Fatal("recovery markers lost")
		}
		closeAPI(t, resumed)
	}
	if probes != 3 {
		t.Fatalf("resume failed to request fresh evidence: %d", probes)
	}
	for _, changed := range [][]int{{3001, 8080}, nil} {
		o.Workbench.Commands.LoopbackPorts = changed
		if changed == nil {
			o.Workbench.Commands.LoopbackControl = ""
		}
		resumed, err := Resume(context.Background(), o, ref)
		if resumed != nil {
			closeAPI(t, resumed)
			t.Fatal("incompatible resume admitted")
		}
		if err == nil {
			t.Fatal("incompatible resume accepted")
		}
	}
	if probes != 3 {
		t.Fatal("incompatible resume ran a proof")
	}
}

func TestWorkbenchSelectedPortsRealProof(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	testenv.RequireLoopback(t)
	o := workbenchOptions(t, nopHandler())
	o.complete = (&scriptedModel{}).complete
	o.Workbench = &Workbench{Commands: &Commands{Loopback: true, LoopbackPorts: []int{31001}, LoopbackControl: os.Getenv("AGENT_HARNESS_TEST_LOOPBACK_CONTROL")}}
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("selected-port workbench proof failed: %v", err)
	}
	defer closeAPI(t, s)
	address, source := s.NetworkControl()
	if address == "" || source == "" {
		t.Fatal("workbench lost proof facts")
	}
	t.Logf("workbench selected ports proved with DNS control %s (%s)", address, source)
}
