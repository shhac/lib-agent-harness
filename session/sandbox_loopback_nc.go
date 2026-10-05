package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

const (
	ncDiagnosticTimeout = 10 * time.Second
	ncDiagnosticMargin  = 12 * time.Second
	// Clients run concurrently. Allow one second for connection/write and one
	// for EOF idle timeout, plus a second for scheduling before the terminator.
	ncCandidateSettleSeconds = 3
)

// ncInterfaceObservation is a private measurement, never an invented errno or
// a prerequisite for the base loopback capability.
type ncInterfaceObservation struct {
	address, operation, outcome string
}

func probeClaudeInterfaceDiagnostics(ctx context.Context, o Options, l *launch) []ncInterfaceObservation {
	addresses, err := sandboxprobe.InterfaceAddresses()
	if err != nil {
		return []ncInterfaceObservation{{operation: "enumeration", outcome: "unavailable"}}
	}
	attempts := sandboxprobe.InterfaceAttempts(addresses, true, true)
	unavailable := make([]ncInterfaceObservation, len(attempts))
	for i, attempt := range attempts {
		unavailable[i] = ncInterfaceObservation{attempt.Address, attempt.Operation, "unavailable"}
	}
	// Do not exhaust the base proof's deadline waiting for optional data.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < ncDiagnosticMargin {
		return unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, ncDiagnosticTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "agent-harness-interface-")
	if err != nil {
		return unavailable
	}
	defer os.RemoveAll(dir)
	script, witnesses, payload, cleanup, err := ncInterfaceCandidate(addresses, dir, true, false)
	if err != nil {
		return unavailable
	}
	defer cleanup()
	output, err := runClaudeLoopbackCanary(ctx, o, l, script, payload)
	if err != nil || ctx.Err() != nil {
		return unavailable
	}
	// Give already-sent datagrams a bounded chance to reach their witnesses.
	select {
	case <-ctx.Done():
		return unavailable
	case <-time.After(100 * time.Millisecond):
	}
	return ncDiagnosticObservations(output, witnesses)
}

func ncDiagnosticObservations(output string, witnesses []*ncInterfaceWitness) []ncInterfaceObservation {
	rows := make([]ncInterfaceObservation, len(witnesses))
	// Validate command completion separately from host delivery. A diagnostic
	// failed bind is recorded, not returned as a session capability refusal.
	validation := make([]*ncInterfaceWitness, len(witnesses))
	for i, w := range witnesses {
		rows[i] = ncInterfaceObservation{w.address, w.operation, "unavailable"}
		validation[i] = &ncInterfaceWitness{address: w.address, operation: w.operation, matched: true}
	}
	if _, err := judgeNCInterfaces(output, validation); err != nil {
		return rows
	}
	for i, w := range witnesses {
		status := "fail"
		for _, line := range strings.Split(output, "\n") {
			if strings.TrimSpace(line) == fmt.Sprintf("nc-interface:%d:ok", i) {
				status = "ok"
			}
		}
		w.mu.Lock()
		matched, payloadMatched := w.matched, w.payloadMatched
		w.mu.Unlock()
		rows[i].outcome = "not observed (nc " + status + "; cause unknown)"
		if matched {
			rows[i].outcome = "observed source"
			if w.operation != "bind" || payloadMatched {
				rows[i].outcome += " and nonce payload"
			}
			host, _, _ := strings.Cut(w.address, "%")
			if net.ParseIP(host).IsUnspecified() {
				rows[i].outcome += " (wildcard: any peer)"
			}
		}
	}
	return rows
}

// Optional diagnostic client: installed dontAsk approval is established by
// runtime observations, never by unit tests.
// A separate host listener witnesses every operation. A process status is never
// reported as an errno. The payload makes UDP observable (empty stdin cannot).
type ncInterfaceWitness struct {
	address, operation, destination string
	port                            int
	mu                              sync.Mutex
	matched                         bool
	tcpNonce                        bool
	payloadMatched                  bool
}

func (w *ncInterfaceWitness) observe(peer net.IP) {
	host, _, _ := strings.Cut(w.address, "%")
	w.mu.Lock()
	defer w.mu.Unlock()
	w.matched = w.matched || net.ParseIP(host).Equal(peer) || net.ParseIP(host).IsUnspecified()
}

func ncInterfaceCandidate(addresses []string, directory string, wildcards, tcpNonce bool) (scriptText string, witnesses []*ncInterfaceWitness, payload string, cleanup func(), err error) {
	var cleanups []func()
	cleanup = func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	file, err := os.CreateTemp(directory, "udp-payload-")
	if err != nil {
		return "", nil, "", cleanup, err
	}
	payload = file.Name()
	cleanups = append(cleanups, func() { os.Remove(file.Name()) })
	nonce := "interface-canary-" + newID() + "\n"
	_, writeErr := file.WriteString(nonce)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return "", nil, "", cleanup, fmt.Errorf("cannot prepare UDP payload")
	}
	var script strings.Builder
	for _, attempt := range sandboxprobe.InterfaceAttempts(addresses, wildcards, true) {
		w := &ncInterfaceWitness{address: attempt.Address, operation: attempt.Operation, tcpNonce: tcpNonce}
		w.destination = "127.0.0.1"
		if strings.Contains(w.address, ":") {
			w.destination = "::1"
		}
		if w.operation == "udp" {
			w.destination = w.address
		}
		endpoint := net.JoinHostPort(w.destination, "0")
		var closeListener func() error
		done := make(chan struct{})
		if w.operation == "bind" {
			listener, err := net.Listen("tcp", endpoint)
			if err != nil {
				return "", nil, "", cleanup, fmt.Errorf("candidate TCP witness unavailable")
			}
			w.port = listener.Addr().(*net.TCPAddr).Port
			closeListener = listener.Close
			go func() {
				defer close(done)
				for {
					c, err := listener.Accept()
					if err != nil {
						return
					}
					// A connection proves host reach independently of payload timing.
					w.observe(c.RemoteAddr().(*net.TCPAddr).IP)
					if tcpNonce {
						_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
						data, readErr := io.ReadAll(io.LimitReader(c, int64(len(nonce)+1)))
						if readErr == nil && string(data) == nonce {
							w.mu.Lock()
							w.payloadMatched = true
							w.mu.Unlock()
						}
					}
					c.Close()
				}
			}()
		} else {
			listener, err := net.ListenPacket("udp", endpoint)
			if err != nil {
				return "", nil, "", cleanup, fmt.Errorf("candidate UDP witness unavailable")
			}
			w.port = listener.LocalAddr().(*net.UDPAddr).Port
			closeListener = listener.Close
			go func() {
				defer close(done)
				b := make([]byte, 128)
				for {
					n, peer, err := listener.ReadFrom(b)
					if err != nil {
						return
					}
					if string(b[:n]) == nonce {
						w.observe(peer.(*net.UDPAddr).IP)
					}
				}
			}()
		}
		cleanups = append(cleanups, func() { closeListener(); <-done })
		flags, input := "-z", "</dev/null"
		if w.operation == "bind" && w.tcpNonce {
			flags, input = "-N", "<"+sandboxprobe.ShellQuote(payload)
		}
		if w.operation != "bind" {
			flags, input = "-u", "<"+sandboxprobe.ShellQuote(payload)
		}
		// A send tests the own-interface destination, without a forced source.
		source := "-s " + sandboxprobe.ShellQuote(w.address)
		if w.operation == "udp" {
			source = ""
		}
		fmt.Fprintf(&script, "nc %s -w1 %s %s %d %s >/dev/null 2>&1 && echo nc-interface:%d:ok || echo nc-interface:%d:fail &\n", flags, source, sandboxprobe.ShellQuote(w.destination), w.port, input, len(witnesses), len(witnesses))
		witnesses = append(witnesses, w)
	}
	// Background the complete AND/OR command, including its result marker.
	// No functions, subshells or wait builtin are needed; incomplete jobs
	// still yield unavailable measurements instead of a successful proof.
	fmt.Fprintf(&script, "sleep %d\necho nc-interface-ran\n", ncCandidateSettleSeconds)
	return script.String(), witnesses, payload, cleanup, nil
}

func judgeNCInterfaces(output string, witnesses []*ncInterfaceWitness) ([]string, error) {
	failure := func(code string) ([]string, error) {
		return nil, &CapabilityError{Engine: harness.Claude, Code: code, Phase: BeforeLaunch}
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == loopbackOutside {
			return failure(CapabilitySandboxNotEnforced)
		}
	}
	statuses := make(map[int]string)
	ran := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == loopbackOutside {
			return failure(CapabilitySandboxNotEnforced)
		}
		if line == "nc-interface-ran" {
			ran = true
		}
		if !strings.HasPrefix(line, "nc-interface:") {
			continue
		}
		var index int
		var status string
		if _, err := fmt.Sscanf(line, "nc-interface:%d:%s", &index, &status); err != nil || index < 0 || index >= len(witnesses) || statuses[index] != "" || (status != "ok" && status != "fail") || line != fmt.Sprintf("nc-interface:%d:%s", index, status) {
			return failure(CapabilitySandboxUnavailable)
		}
		statuses[index] = status
	}
	if !ran || len(statuses) != len(witnesses) {
		return failure(CapabilitySandboxUnavailable)
	}
	outcomes := make([]string, len(witnesses))
	for i, w := range witnesses {
		w.mu.Lock()
		matched, payloadMatched := w.matched, w.payloadMatched
		w.mu.Unlock()
		outcomes[i] = "unconfirmed (nc " + statuses[i] + ")"
		if matched {
			outcomes[i] = "observed source"
			if w.operation != "bind" || payloadMatched {
				outcomes[i] += " and payload"
			}
		}
		if w.operation == "bind" && matched && !w.tcpNonce {
			outcomes[i] = "observed source"
		}
		host, _, _ := strings.Cut(w.address, "%")
		if matched && net.ParseIP(host).IsUnspecified() {
			outcomes[i] += " (wildcard: any peer)"
		}
		if w.operation != "udp" && !matched {
			if statuses[i] == "fail" {
				return failure(CapabilityLoopbackClaimChanged)
			}
			return failure(CapabilitySandboxUnavailable)
		}
	}
	return outcomes, nil
}
