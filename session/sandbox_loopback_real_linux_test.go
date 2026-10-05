//go:build linux

package session

import (
	"context"
	stdflag "flag"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

// Collect installed-runtime evidence without inference. Each launch has its
// own deadline, including cold startup; all evidence is logged before assertions.
func TestClaudeLoopbackInterfaceRealProofLinux(t *testing.T) {
	selected := stdflag.Lookup("test.run").Value.String()
	if selected != "TestClaudeLoopbackInterfaceRealProofLinux" && selected != "^TestClaudeLoopbackInterfaceRealProofLinux$" && os.Getenv("AGENT_HARNESS_TEST_CLAUDE_INTERFACE_PROOF") != "1" {
		t.Skip("owner-only installed Claude proof; select exact name")
	}
	if os.Getenv("AGENT_HARNESS_TEST_NO_SKIP") != "1" {
		t.Fatal("requires AGENT_HARNESS_TEST_NO_SKIP=1")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("installed Claude Code required")
	}
	if err := checkClaudeSandboxPrerequisites("linux", Options{Provider: harness.Provider{Engine: harness.Claude}, Sandbox: &Sandbox{}}, exec.LookPath); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{binary, "bwrap", "/usr/bin/python3"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		version, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
		cancel()
		if err != nil {
			if command == "/usr/bin/python3" {
				t.Log("unproved: optional Python client version unavailable: ", err)
				continue
			}
			t.Fatalf("%s version unavailable: %v", command, err)
		}
		t.Logf("%s: %s", command, strings.TrimSpace(string(version)))
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(release))
	raw := sandboxOptions(t, harness.Claude, binary, true)
	raw.Provider.CLI.Home = t.TempDir()
	raw.Sandbox.Loopback = true
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	productionErr := VerifySandbox(ctx, raw)
	t.Logf("production VerifySandbox outcome: %v", productionErr)
	cancel()
	o, err := normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	l := &launch{extra: sandboxArgs(o)}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	witness, ok := offMachineWitness(ctx)
	cancel()
	if !ok {
		t.Fatal("off-machine witness unavailable; no scope claim possible")
	}
	listener, wait, reached, err := countingListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { listener.Close(); wait.Wait() }()
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	run := func(script string, payload ...string) (string, bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		output, received, err := runClaudeLoopbackCanaryObserved(ctx, o, l, script, payload...)
		if err == nil && ctx.Err() != nil {
			err = &CapabilityError{Engine: harness.Claude, Code: CapabilityProbeTimeout, Phase: BeforeLaunch}
		}
		return output, received, err
	}
	output, received, err := run(loopbackCanary(listener.Addr().(*net.TCPAddr).Port, witness, 443, port))
	if err != nil {
		if e, ok := err.(*CapabilityError); !ok || e.Code != CapabilityProbeTimeout {
			t.Fatal("host-local base setup failed: ", err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	scopeCtx := context.Background()
	if err != nil {
		cancelled, cancel := context.WithCancel(scopeCtx)
		cancel()
		scopeCtx = cancelled
	}
	scope, baseErr := judgeClaudeLinuxScope(scopeCtx, output, reached())
	t.Logf("candidate base scope=%s tool-result=%t outcome=%v", scope, received, baseErr)
	key, keyErr := realClaudeProofKey(raw)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	verified.mu.Lock()
	recordedScope := verified.loopback[key].scope
	verified.mu.Unlock()
	t.Logf("production recorded scope=%s", recordedScope)
	// Optional diagnostics are report-only, including local setup and launch
	// refusal. Production scope assertions below remain authoritative.
	ncOutput := func() string {
		addresses, err := sandboxprobe.InterfaceAddresses()
		if err != nil || len(addresses) == 0 {
			t.Log("unproved: host interface addresses unavailable")
			return ""
		}
		attempts := sandboxprobe.InterfaceAttempts(addresses, true, true)
		for i := range attempts {
			attempts[i].NamespaceScope = strings.Contains(attempts[i].Address, "%")
		}
		_, witnesses, payload, cleanup, err := ncInterfaceCandidate(addresses, t.TempDir(), true, true)
		if err != nil {
			t.Log("unproved: Python witness setup unavailable: ", err)
			return ""
		}
		defer cleanup()
		nonce, err := os.ReadFile(payload)
		if err != nil {
			t.Log("unproved: diagnostic payload unavailable: ", err)
			return ""
		}
		script := claudeLinuxInterfaceCanary(attempts, witnesses, string(nonce))
		if !flatCanary(script) {
			t.Log("unproved: exact Python command is not flat")
			return ""
		}
		output, received, launchErr := run(script)
		time.Sleep(100 * time.Millisecond)
		matched := claudeWitnessMatched(witnesses)
		rows, summary, measureErr := judgeClaudeLinuxInterfaces(output, attempts, matched, scope)
		// Never discard the redacted table or completion facts on failure.
		t.Logf("Python tool-result=%t socket-results=%d canary-ran=%t namespace-report=%t witness-ran=%t launch-outcome=%v", received, strings.Count(output, "interface-result:"), strings.Contains(output, "interface-canary-ran\n"), strings.Contains(output, "namespace-interfaces:"), strings.Contains(output, "interface-witness-ran\n"), launchErr)
		if !received || !strings.Contains(output, "interface-canary-ran\n") {
			t.Log("client auto-allow not established; attempts unavailable, not denied")
		}
		for i, attempt := range attempts {
			errno := "unavailable"
			if len(rows) == len(attempts) {
				errno = claudeLinuxErrnoClass(rows[i].Errno)
			}
			w := witnesses[i]
			w.mu.Lock()
			delivered := w.matched
			w.mu.Unlock()
			t.Logf("%s | %s | %s | host-reach=%t", claudeInterfaceAddressClass(attempt.Address), attempt.Operation, errno, delivered)
		}
		if names, ok := claudeLinuxNamespaceNames(output); ok {
			t.Logf("namespace interfaces: %v", names)
		}
		// The judge's unavailable error is not a CLI launch refusal. Print the
		// measurement summary separately from launch-outcome above.
		t.Logf("interface summary=%s", summary)
		if measureErr != nil {
			if e, ok := measureErr.(*CapabilityError); ok && e.Code == CapabilitySandboxNotEnforced {
				t.Log("contradiction observed: interface judge detected host reach or off-machine output; diagnostic only")
			} else {
				t.Log("unproved: interface measurement not established")
			}
		}
		// Fresh sockets and a new nonce keep the fallback independent of Python.
		ncScript, ncWitnesses, ncPayload, ncCleanup, err := ncInterfaceCandidate(addresses, t.TempDir(), true, true)
		if err != nil {
			t.Log("unproved: nc witness setup unavailable: ", err)
			return ""
		}
		defer ncCleanup()
		ncOutput, ncReceived, ncErr := run(ncScript, ncPayload)
		t.Logf("nc tool-result=%t launch-outcome=%v", ncReceived, ncErr)
		for _, row := range ncDiagnosticObservations(ncOutput, ncWitnesses) {
			t.Logf("nc witness | %s | %s", row.operation, row.outcome)
		}
		if scope != "host-shared" && claudeWitnessMatched(ncWitnesses) {
			t.Log("contradiction observed: nc host witness contradicts per-command scope; diagnostic only")
		}
		if launchErr != nil || !strings.Contains(output, "interface-witness-ran\n") {
			t.Log("unproved: Python measurement did not complete; diagnostic only")
		}
		return ncOutput
	}()
	// Off-machine evidence remains fatal even in nc diagnostics: it directly
	// contradicts the base network restriction, unlike unavailable bind data.
	if err := judgeClaudeLinuxOwnerProof(productionErr, baseErr, scope, recordedScope, ncOutput); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeLinuxOwnerCommandIsFlat(t *testing.T) {
	witnesses := []*ncInterfaceWitness{{address: "192.0.2.1", destination: "127.0.0.1", port: 1234, operation: "bind"}, {address: "fe80::1%eth0", destination: "::1", port: 1235, operation: "udp-bind"}, {address: "0.0.0.0", destination: "0.0.0.0", port: 1236, operation: "udp"}}
	attempts := sandboxprobe.InterfaceAttempts([]string{"192.0.2.1", "fe80::1%eth0"}, true, true)
	if !flatCanary(claudeLinuxInterfaceCanary(attempts, witnesses, "interface-canary-fixed\n")) {
		t.Fatal("owner command including witness tail is not flat")
	}
}
