package session

import (
	"context"
	"errors"
	"reflect"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestSandboxErrorTranslation(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}}
	var pairs [][2]error
	for _, tc := range []struct{ operation, code, reason string }{
		{"workbench", sandbox.RefusedWorkbenchMountCheck, "the runtime cannot check workspace mount identity"},
		{"work_dir", sandbox.RefusedWorkDir, "the workspace could not be opened"},
		{"start", sandbox.RefusedNotOffered, "unavailable"},
		{"runtime_home", sandbox.RefusedRuntimeHome, "unavailable"},
		{"read", sandbox.RefusedSandboxRead, "unavailable"},
		{"workbench", sandbox.RefusedLimit, "unavailable"},
		{"workbench", sandbox.RefusedConflict, "unavailable"},
	} {
		pairs = append(pairs, [2]error{&sandbox.RefusalError{Operation: tc.operation, Code: tc.code, Capability: harness.Capability{Availability: harness.Unsupported, Reason: tc.reason}}, refuse(o, tc.operation, tc.code, tc.reason)})
	}
	for _, code := range []string{sandbox.CapabilitySandboxToolMissing, sandbox.CapabilitySandboxToolOutdated, sandbox.CapabilitySandboxNamespacesUnavailable, sandbox.CapabilitySandboxUnavailable, sandbox.CapabilitySandboxNotEnforced, sandbox.CapabilityProbeTimeout} {
		for _, tools := range [][]string{nil, {"bwrap"}} {
			pairs = append(pairs, [2]error{&sandbox.ProofError{Code: code, Tools: tools}, &CapabilityError{Engine: harness.OpenAICompatible, Code: code, Phase: BeforeLaunch, Tools: tools}})
		}
	}
	for _, code := range []string{sandbox.WorkspaceIOStuck, sandbox.CommandStartFailed, sandbox.CommandOutcomeUnknown, sandbox.CommandProcessLimit, sandbox.CommandCleanupUnknown, sandbox.CommandSandboxClosed} {
		pairs = append(pairs, [2]error{&sandbox.CommandError{Code: code}, &TurnError{Engine: harness.OpenAICompatible, Code: code}})
	}
	for _, code := range []string{sandbox.StateUnusable, sandbox.StateLocked} {
		pairs = append(pairs, [2]error{&sandbox.StateError{Code: code}, stateError(code)})
	}
	for _, pair := range pairs {
		input, want := pair[0], pair[1]
		got := fromSandbox(input, o)
		if !reflect.DeepEqual(got, want) || got.Error() != want.Error() {
			t.Fatalf("%T translation: %#v != %#v", input, got, want)
		}
		facts, ok := harness.ErrorFacts(got)
		before, beforeOK := harness.ErrorFacts(want)
		raw, rawOK := harness.ErrorFacts(input)
		if !ok || !beforeOK || !rawOK || !reflect.DeepEqual(facts, before) || !reflect.DeepEqual(facts, raw) {
			t.Fatalf("facts drift: %+v %+v %+v", facts, before, raw)
		}
		if input.Error() != want.Error() {
			t.Fatalf("sandbox message drift: %s != %s", input, want)
		}
		switch want.(type) {
		case *UnsupportedError, *CapabilityError:
			if !errors.Is(got, ErrUnsupported) {
				t.Fatal(got)
			}
		case *StateError:
			if want.(*StateError).Code == StateLocked && !errors.Is(got, ErrLeaseHeld) {
				t.Fatal(got)
			}
		case *TurnError:
			if !errors.Is(got, ErrTurnFailed) {
				t.Fatal(got)
			}
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, ErrClosed, errors.New("other"), sandbox.ErrWriteUnknown} {
		if fromSandbox(err, o) != err {
			t.Fatal("unrelated error changed")
		}
	}
	if fromSandbox(sandbox.ErrClosed, o) != ErrClosed {
		t.Fatal("closed sentinel drift")
	}
	if !workspaceStuck(fromSandbox(&sandbox.CommandError{Code: sandbox.WorkspaceIOStuck}, o)) {
		t.Fatal("stuck workspace lost")
	}
}

func TestSandboxOpenRefusalIdentity(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, WorkDir: t.TempDir() + "/missing", Workbench: &Workbench{}, Restriction: &Restriction{Tools: ToolHost{}}}
	_, err := openWorkspace(o, newID())
	var refused *UnsupportedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrUnsupported) || refused.Code != RefusedWorkDir || refused.Operation != "work_dir" {
		t.Fatal(err)
	}
}
