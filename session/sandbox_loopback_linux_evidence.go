package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

const claudeLinuxInCommandLoopbackReason = ClaudeLinuxInCommandLoopbackFailed
const claudeLinuxWiderScopeReason = ClaudeLinuxLoopbackScopeWiderThanClaimed

func claudeWitnessMatched(witnesses []*ncInterfaceWitness) bool {
	matched := false
	for _, w := range witnesses {
		w.mu.Lock()
		matched = matched || w.matched
		w.mu.Unlock()
	}
	return matched
}

func claudeLinuxErrnoClass(errno int) string {
	switch errno {
	case 0:
		return "succeeded (0)"
	case 1, 13:
		return fmt.Sprintf("denied (%d)", errno)
	case 99:
		return "address unavailable (99)"
	default:
		return fmt.Sprintf("other (%d)", errno)
	}
}

func claudeLinuxNamespaceNames(output string) ([]string, bool) {
	var names []string
	found := false
	for _, line := range strings.Split(output, "\n") {
		if data, ok := strings.CutPrefix(line, "namespace-interfaces:"); ok {
			if found || len(data) > 8192 || json.Unmarshal([]byte(data), &names) != nil {
				return nil, false
			}
			found = true
		}
	}
	if !found || len(names) == 0 || len(names) > 128 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, name := range names {
		if len(name) == 0 || len(name) > 64 || seen[name] {
			return nil, false
		}
		seen[name] = true
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
				return nil, false
			}
		}
	}
	return names, true
}

// The witness tail uses double quotes exclusively, just like the pinned socket
// client. ShellQuote therefore adds no embedded quote transitions to -c.
func claudeLinuxInterfaceCanary(attempts []sandboxprobe.InterfaceAttempt, witnesses []*ncInterfaceWitness, nonce string) string {
	var destinations []map[string]any
	for _, w := range witnesses {
		destinations = append(destinations, map[string]any{"source": w.address, "destination": w.destination, "port": w.port, "operation": w.operation})
	}
	encoded, _ := json.Marshal(destinations)
	if destinations == nil {
		encoded = []byte("[]")
	}
	tail := "for w in json.loads(" + strconv.Quote(string(encoded)) + "):\n" + ` s=None
 try:
  family=socket.AF_INET6 if ":" in w["source"] else socket.AF_INET
  s=socket.socket(family,socket.SOCK_STREAM if w["operation"]=="bind" else socket.SOCK_DGRAM)
  s.settimeout(0.3)
  if w["operation"]!="udp": s.bind(socket.getaddrinfo(w["source"],0,family,0,0,socket.AI_NUMERICHOST)[0][4])
  destination=socket.getaddrinfo(w["destination"],w["port"],family,0,0,socket.AI_NUMERICHOST)[0][4]
  if w["operation"]=="bind":
   s.connect(destination)
   s.sendall(` + strconv.Quote(nonce) + `.encode())
  else: s.sendto(` + strconv.Quote(nonce) + `.encode(),destination)
 except OSError: pass
 finally:
  if s is not None: s.close()
print("interface-witness-ran",flush=True)
`
	return sandboxprobe.InterfacePythonCanary("/usr/bin/python3", attempts, true, tail)
}

// The Linux base proof measures its own listener separately from host scope.
func judgeClaudeLinuxScope(ctx context.Context, output string, reached bool) (string, error) {
	lines := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	fail := func(code string) (string, error) {
		return "", &CapabilityError{Engine: harness.Claude, Code: code, Phase: BeforeLaunch}
	}
	if lines[loopbackOutside] {
		return fail(CapabilitySandboxNotEnforced)
	}
	if ctx.Err() != nil {
		return fail(CapabilityProbeTimeout)
	}
	if !lines[canaryRan] || lines[canaryNoClient] || lines[loopbackReached] != reached {
		return fail(CapabilitySandboxUnavailable)
	}
	if !lines[loopbackBound] {
		return "", &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch, Reason: claudeLinuxInCommandLoopbackReason}
	}
	if reached {
		return "host-shared", nil
	}
	return "per-command", nil
}

func judgeClaudeLinuxInterfaces(output string, attempts []sandboxprobe.InterfaceAttempt, witness bool, scope string) ([]sandboxprobe.InterfaceObservation, string, error) {
	fail := func(code string) ([]sandboxprobe.InterfaceObservation, string, error) {
		return nil, "unavailable", &CapabilityError{Engine: harness.Claude, Code: code, Phase: BeforeLaunch}
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == loopbackOutside {
			rows, _ := sandboxprobe.JudgeInterfaceAttempts(output, attempts, sandboxprobe.InterfaceAllLocal)
			return rows, "unavailable", &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
		}
	}
	rows, outcome := sandboxprobe.JudgeInterfaceAttempts(output, attempts, sandboxprobe.InterfaceAllLocal)
	// A host delivery contradicts a per-command scope, but is expected when the
	// base proof observed host-shared loopback. Preserve complete rows on escape.
	if witness && scope != "host-shared" {
		return rows, "all-interfaces", &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	}
	if outcome != "" {
		_, _, err := fail(string(outcome))
		detail := "measurement unavailable: incomplete, garbled or duplicate socket results"
		if !strings.Contains(output, "interface-result:") {
			detail = "measurement unavailable: no interface-result lines; client auto-allow not established"
		} else if !strings.Contains(output, "interface-canary-ran\n") {
			detail = "measurement unavailable: no interface-canary-ran marker"
		}
		return nil, detail, err
	}
	names, validNames := claudeLinuxNamespaceNames(output)
	if !validNames {
		_, _, err := fail(CapabilitySandboxUnavailable)
		return nil, "measurement unavailable: missing or invalid namespace report", err
	}
	private, exposed, hosts := true, witness, 0
	for _, row := range rows {
		if row.Operation == "udp" || row.Address == "0.0.0.0" || row.Address == "::" {
			continue
		}
		hosts++
		private = private && row.Errno == 99
		exposed = exposed || row.Errno == 0
	}
	summary := "mixed/unavailable"
	// An offline host and sends alone cannot establish namespace confinement.
	if hosts > 0 && private && validNames && len(names) == 1 && names[0] == "lo" {
		summary = "private-namespace"
	}
	if exposed {
		summary = "all-interfaces"
	}
	return rows, summary, nil
}

// Optional interface measurements do not gate the owner's production check.
// Off-machine escape remains fatal because it contradicts the network contract.
func judgeClaudeLinuxOwnerProof(productionErr, baseErr error, scope, recordedScope, ncOutput string) error {
	if baseErr != nil {
		if e, ok := baseErr.(*CapabilityError); ok && e.Code == CapabilitySandboxNotEnforced {
			return baseErr
		}
	}
	for _, line := range strings.Split(ncOutput, "\n") {
		if strings.TrimSpace(line) == loopbackOutside {
			return &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
		}
	}
	if productionErr != nil || baseErr != nil || scope != "per-command" || recordedScope != "per-command" {
		return fmt.Errorf("documented per-command contract not proved: production=%v base=%v scope=%s recorded=%s", productionErr, baseErr, scope, recordedScope)
	}
	return nil
}

// The owner-selected contract supplies the claimed scope. A narrower
// observation is safe; a host-shared observation must never widen per-command.
func judgeClaudeLinuxClaim(ctx context.Context, output string, reached bool, claimed string) (loopbackEvidence, error) {
	scope, err := judgeClaudeLinuxScope(ctx, output, reached)
	if err != nil {
		return loopbackEvidence{}, err
	}
	if claimed == "per-command" && scope == "host-shared" {
		return loopbackEvidence{}, &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch, Reason: claudeLinuxWiderScopeReason}
	}
	return loopbackEvidence{scope: scope}, nil
}

// Admission applies the owner-observed Linux scope without relaxing macOS.
// Explicit GOOS keeps every rule testable on every host.
func judgeClaudeLoopbackForPlatform(ctx context.Context, goos, claimed, output string, reached bool, attempts []sandboxprobe.InterfaceAttempt) (loopbackEvidence, error) {
	if goos == "linux" {
		return judgeClaudeLinuxClaim(ctx, output, reached, claimed)
	}
	evidence, err := judgeClaudeLoopback(output, reached, attempts)
	if e, ok := err.(*CapabilityError); ok && e.Code == CapabilitySandboxNotEnforced {
		return loopbackEvidence{}, err
	}
	if ctx.Err() != nil {
		return loopbackEvidence{}, &CapabilityError{Engine: harness.Claude, Code: CapabilityProbeTimeout, Phase: BeforeLaunch}
	}
	return evidence, err
}
