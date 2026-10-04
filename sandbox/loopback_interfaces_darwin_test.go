//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run outside any enclosing sandbox, with AGENT_HARNESS_TEST_NO_SKIP=1.
// A rejected syntax is not a confinement proof; every accepted candidate must
// positively execute its attempt client and show the advertised bind exposure.
func TestSeatbeltLoopbackRuleForms(t *testing.T) {
	requireWorkbenchSeatbeltExecution(t)
	addresses, err := interfaceAddresses()
	if err != nil {
		t.Fatal(err)
	}
	attempts := interfaceAttempts(addresses, true, true)
	t.Logf("host interface count: %d (wildcards tested separately)", len(addresses))
	for _, form := range []string{"ip", "ip4", "ip6", "tcp", "udp"} {
		t.Run(form, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			profile := fmt.Sprintf("(version 1)(allow default)(deny network*)(allow network-bind (local %s \"localhost:*\"))(allow network-inbound (local %s \"localhost:*\"))(allow network-outbound (remote ip \"localhost:*\"))", form, form)
			// Compile separately: a rejected candidate cannot masquerade as a
			// client failure or a successful negative networking canary.
			out, err := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", "echo profile-accepted").CombinedOutput()
			if err != nil {
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				t.Logf("%s: profile syntax refused: %s", form, strings.TrimSpace(string(out)))
				if form == "ip" {
					t.Fatal(err)
				}
				return
			}
			out, err = exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", interfacePerlCanary(attempts)).CombinedOutput()
			if err != nil {
				t.Fatalf("client failed: %v: %s", err, out)
			}
			observations, err := judgeInterfaceAttempts(string(out), attempts, interfaceAllLocal)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range observations {
				t.Logf("%s %s %s errno=%d", form, o.Operation, interfaceAddressClass(o.Address), o.Errno)
			}
			bindAllowed := false
			for _, o := range observations {
				if o.Operation != "udp" && o.Errno == 0 {
					bindAllowed = true
				}
			}
			if !bindAllowed {
				t.Fatal("rule-form bind finding changed: no positive interface/wildcard bind; review this form before claiming confinement")
			}
			_, strict := judgeInterfaceAttempts(string(out), attempts, interfaceLoopbackOnly)
			var p *ProofError
			if !errors.As(strict, &p) || p.Code != CapabilitySandboxNotEnforced {
				t.Fatalf("rule-form finding changed: %v", strict)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	profile := "(version 1)(allow default)(deny network*)(allow network-bind (local ip \"127.0.0.1:*\"))"
	out, err := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", "echo literal-accepted").CombinedOutput()
	if ctx.Err() != nil || err == nil || !literalHostRejected(string(out)) {
		t.Fatalf("literal rule-form finding changed: %v: %s", err, out)
	}
	t.Logf("literal 127.0.0.1: profile refused: %s", strings.TrimSpace(string(out)))
}

func TestWorkbenchLoopbackRealInterfaceProof(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	o := commandSandboxOptions(t, true)
	p, err := Prove(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if p.NetworkDetail() == "" || len(p.NetworkObservations()) < 6 {
		t.Fatal("missing interface observations")
	}
	t.Log(p.NetworkDetail())
	for _, observation := range p.NetworkObservations() {
		logInterfaceObservation(t, observation)
	}
	if err := requireInterfaceExposure(p.NetworkObservations()); err != nil {
		t.Fatal(err)
	}
	again, err := Prove(context.Background(), o)
	if err != nil || again.NetworkDetail() != p.NetworkDetail() {
		t.Fatalf("cached proof lost evidence: %v", err)
	}
}
