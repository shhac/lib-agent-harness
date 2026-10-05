package session

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestProcessInspectionCommandAdapter(t *testing.T) {
	o := Options{Workbench: &Workbench{Commands: &Commands{ProcessInspection: true}}}
	if !commandOptions(o).ProcessInspection {
		t.Fatal("inspection request lost")
	}
	o.Workbench.Commands.ProcessInspection = false
	if commandOptions(o).ProcessInspection {
		t.Fatal("zero value enabled inspection")
	}
	base, _ := json.Marshal(workbenchDigest(o))
	o.Workbench.Commands.ProcessInspection = true
	requested, _ := json.Marshal(workbenchDigest(o))
	if string(base) == string(requested) {
		t.Fatal("inspection power missing from reference")
	}
	o.Workbench.Commands.ProcessInspection = false
	zero, _ := json.Marshal(workbenchDigest(o))
	if string(base) != string(zero) {
		t.Fatal("zero value changed reference")
	}
}

func TestProcessInspectionRefusalTranslation(t *testing.T) {
	for _, code := range []string{sandbox.CapabilityProcessInspectionUnavailable, sandbox.CapabilitySandboxNotEnforced, sandbox.CapabilityProbeTimeout} {
		err := fromSandbox(&sandbox.ProofError{Code: code, Step: sandbox.ProofStepProcessInspection}, Options{})
		if code == sandbox.CapabilitySandboxNotEnforced && !strings.Contains(err.Error(), "process inspection could see processes outside") {
			t.Fatal(err)
		}
		facts, _ := harness.ErrorFacts(err)
		if facts.Code != code || facts.ProofStep != sandbox.ProofStepProcessInspection {
			t.Fatalf("%+v", facts)
		}
	}
}

func TestProcessInspectionHostedEarlyRefusal(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	_, err := normalizeWorkbench(Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, Workbench: &Workbench{Commands: &Commands{ProcessInspection: true}}})
	var refused *UnsupportedError
	if !errors.As(err, &refused) || refused.Code != sandbox.RefusedProcessInspectionUnenforceable {
		t.Fatalf("%v", err)
	}
	facts, _ := harness.ErrorFacts(err)
	if facts.Family != harness.FailureCapability {
		t.Fatalf("%+v", facts)
	}
}
