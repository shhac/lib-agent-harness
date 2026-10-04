package sandbox

import (
	"fmt"
	"net"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

// InterfaceObservation records structural outcomes, never raw client output.
// Denied means EPERM/EACCES only; unavailable addresses prove nothing.
type InterfaceObservation = sandboxprobe.InterfaceObservation
type interfaceAttempt = sandboxprobe.InterfaceAttempt
type interfaceJudgeMode = sandboxprobe.InterfaceJudgeMode

const (
	interfaceLoopbackOnly     = sandboxprobe.InterfaceLoopbackOnly
	interfaceAllLocal         = sandboxprobe.InterfaceAllLocal
	interfacePrivateNamespace = sandboxprobe.InterfacePrivateNamespace
)

var interfaceAddresses = sandboxprobe.InterfaceAddresses
var probeInterface = sandboxprobe.ProbeInterface
var normalizeInterfaceAddresses = sandboxprobe.NormalizeInterfaceAddresses
var interfaceAttempts = sandboxprobe.InterfaceAttempts
var interfaceAttemptsAtPort = sandboxprobe.InterfaceAttemptsAtPort
var namespaceInterfaceAttempts = sandboxprobe.NamespaceInterfaceAttempts
var addressUnavailableErrno = sandboxprobe.AddressUnavailableErrno
var interfaceExposureDetail = sandboxprobe.InterfaceExposureDetail

func judgeInterfaceAttempts(output string, attempts []interfaceAttempt, mode interfaceJudgeMode) ([]InterfaceObservation, error) {
	observations, outcome := sandboxprobe.JudgeInterfaceAttempts(output, attempts, mode)
	if outcome != "" {
		return nil, &ProofError{Code: string(outcome)}
	}
	return observations, nil
}
func requireInterfaceExposure(observations []InterfaceObservation) error {
	if outcome := sandboxprobe.RequireInterfaceExposure(observations); outcome != "" {
		return &ProofError{Code: string(outcome)}
	}
	return nil
}
func localOnlyDetail(o Options, observations []InterfaceObservation) string {
	if !o.LoopbackLocalOnly {
		return ""
	}
	if len(observations) == 0 {
		return "private namespace loopback proved; no interface addresses to test"
	}
	return "private namespace loopback proved; host interface binds unavailable"
}

// An address may vanish after enumeration; try the remaining interfaces.
func listenInterfaceWitness(attempts []interfaceAttempt, listen func(string, string) (net.Listener, error)) (string, net.Listener, error) {
	for _, a := range attempts {
		host, _, _ := strings.Cut(a.Address, "%")
		if a.Operation != "bind" || net.ParseIP(host).IsUnspecified() {
			continue
		}
		l, err := listen("tcp", net.JoinHostPort(a.Address, "0"))
		if err == nil {
			return a.Address, l, nil
		}
	}
	return "", nil, fmt.Errorf("no interface witness address available")
}
