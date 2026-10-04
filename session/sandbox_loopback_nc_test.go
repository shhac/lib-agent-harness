//go:build !windows

package session

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestNCInterfaceCandidate(t *testing.T) {
	testenv.RequireLoopback(t)
	nc, err := exec.LookPath("nc")
	if err != nil {
		t.Log("optional nc diagnostic unavailable")
		return
	}
	_ = nc
	script, witnesses, _, cleanup, err := ncInterfaceCandidate([]string{"127.0.0.1"}, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !flatCanary(script) {
		t.Fatal("candidate exceeds flat command set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Log("optional nc variant cannot complete candidate")
		return
	}
	time.Sleep(100 * time.Millisecond)
	for i, w := range witnesses {
		if !strings.Contains(string(output), fmt.Sprintf("nc-interface:%d:ok\n", i)) {
			t.Log("optional nc command did not succeed")
			continue
		}
		w.mu.Lock()
		matched := w.matched
		w.mu.Unlock()
		if !matched {
			t.Fatalf("successful nc %s had no matching source/nonce witness", w.operation)
		}
	}
}

func TestNCCandidateRuntimeBudget(t *testing.T) {
	testenv.RequireLoopback(t)
	// Ten addresses stand in for a Mac with several physical/virtual interfaces.
	// IPv4 loopback avoids making IPv6 or commercial CLI availability a CI need.
	addresses := make([]string, 10)
	for i := range addresses {
		addresses[i] = "127.0.0.1"
	}
	script, witnesses, _, cleanup, err := ncInterfaceCandidate(addresses, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(witnesses) != 30 {
		t.Fatalf("attempt count: %d", len(witnesses))
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "nc ") && !strings.HasSuffix(line, " &") {
			t.Fatal("serial nc wait exceeds diagnostic budget")
		}
	}
	if !flatCanary(script) || strings.Count(script, "sleep ") != 1 || !strings.Contains(script, fmt.Sprintf("sleep %d\necho nc-interface-ran", ncCandidateSettleSeconds)) {
		t.Fatal("candidate lost bounded flat settling shape")
	}
	// Reserve six seconds for the second CLI launch and one for result delivery.
	if time.Duration(ncCandidateSettleSeconds)*time.Second+7*time.Second > ncDiagnosticTimeout || ncDiagnosticMargin < ncDiagnosticTimeout+2*time.Second {
		t.Fatal("candidate exceeds launch/settlement budget")
	}
}

func TestEscapeSurvivesCancelledSettlement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := &verificationCache{}
	escape := &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	if err := cache.settleSandbox(ctx, harness.Claude, "escaped", loopbackEvidence{}, escape); err != escape || cache.holds("escaped") {
		t.Fatal("timeout masked escape or published success")
	}
}

func TestCancelledClaudeLoopbackSettlement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, output := range []string{"", "loopback\nbound\ncanary-ran\n"} {
		evidence, err := settleClaudeLoopback(ctx, output, true)
		if capabilityCode(t, err) != CapabilityProbeTimeout || len(evidence.observations) != 0 {
			t.Fatal("cancelled output became proof")
		}
	}
	_, err := settleClaudeLoopback(ctx, "outside\n", false)
	if capabilityCode(t, err) != CapabilitySandboxNotEnforced {
		t.Fatal("deadline masked escape")
	}
}

func TestJudgeNCInterfaces(t *testing.T) {
	witnesses := []*ncInterfaceWitness{{address: "127.0.0.1", operation: "bind", matched: true}, {address: "127.0.0.1", operation: "udp-bind", matched: true}, {address: "::", operation: "udp"}}
	complete := "nc-interface:0:ok\nnc-interface:1:ok\nnc-interface:2:fail\nnc-interface-ran\n"
	if outcomes, err := judgeNCInterfaces(complete, witnesses); err != nil || !strings.Contains(outcomes[2], "unconfirmed") {
		t.Fatalf("%v %v", outcomes, err)
	}
	for _, tc := range []struct{ output, code string }{
		{"nc-interface:bad\noutside\n", CapabilitySandboxNotEnforced},
		{"nc-interface-ran\n", CapabilitySandboxUnavailable},
		{complete + "nc-interface:0:ok\n", CapabilitySandboxUnavailable},
		{strings.ReplaceAll(complete, ":0:ok", ":0:ok trailing"), CapabilitySandboxUnavailable},
		{strings.ReplaceAll(complete, "nc-interface-ran", ""), CapabilitySandboxUnavailable},
		{strings.ReplaceAll(complete, ":0:ok", ":0:bogus"), CapabilitySandboxUnavailable},
	} {
		_, err := judgeNCInterfaces(tc.output, witnesses)
		if capabilityCode(t, err) != tc.code {
			t.Fatal("wrong precedence or completeness outcome")
		}
	}
	witnesses[0].matched = false
	if _, err := judgeNCInterfaces(complete, witnesses); capabilityCode(t, err) != CapabilitySandboxUnavailable {
		t.Fatal("missing host evidence accepted")
	}
	if _, err := judgeNCInterfaces(strings.ReplaceAll(complete, ":0:ok", ":0:fail"), witnesses); capabilityCode(t, err) != CapabilityLoopbackClaimChanged {
		t.Fatal("failed bind accepted")
	}
}

func TestNCOptionalMeasurementsDoNotInvalidateBase(t *testing.T) {
	witnesses := []*ncInterfaceWitness{{address: "0.0.0.0", operation: "bind", matched: true}, {address: "127.0.0.1", operation: "udp-bind"}, {address: "127.0.0.1", operation: "udp", matched: true}}
	output := "nc-interface:0:ok\nnc-interface:1:fail\nnc-interface:2:ok\nnc-interface-ran\n"
	rows := ncDiagnosticObservations(output, witnesses)
	if len(rows) != 3 || !strings.Contains(rows[0].outcome, "wildcard: any peer") || !strings.Contains(rows[1].outcome, "cause unknown") || !strings.Contains(rows[2].outcome, "nonce payload") {
		t.Fatalf("incorrect observations: %+v", rows)
	}
	for _, incomplete := range []string{"", "nc-interface-ran", output + "nc-interface:1:fail\n"} {
		for _, row := range ncDiagnosticObservations(incomplete, witnesses) {
			if row.outcome != "unavailable" {
				t.Fatal("partial output became observation")
			}
		}
	}
	cache := &verificationCache{seen: map[string]bool{}}
	rows = ncDiagnosticObservations("", witnesses)
	evidence := loopbackEvidence{interfaces: rows}
	if err := cache.settleSandbox(context.Background(), harness.Claude, "base", evidence, nil); err != nil || !cache.holds("base") {
		t.Fatalf("unavailable diagnostics removed capability: %v", err)
	}
	rows[0].outcome = "mutated"
	if cache.loopback["base"].interfaces[0].outcome != "unavailable" {
		t.Fatal("cached observations alias caller")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cache.settleSandbox(ctx, harness.Claude, "cancelled-nc", evidence, nil); err == nil || cache.holds("cancelled-nc") {
		t.Fatal("cancelled positive proof recorded")
	}
}
