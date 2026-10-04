package sandboxprobe

import (
	"fmt"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// InterfaceObservation records structural outcomes, never raw client output.
// Denied means EPERM/EACCES only; unavailable addresses prove nothing.
type InterfaceObservation struct {
	Address   string
	Operation string
	Errno     int
	// NamespaceScope means the host's IPv6 scope was replaced by private lo.
	NamespaceScope bool
}
type InterfaceAttempt struct {
	Address, Operation string
	Port               int
	NamespaceScope     bool
}

// InterfaceAddresses retains scoped IPv6 link-local addresses. Enumeration is
// outside the sandbox: private namespaces must not substitute their own list.
func InterfaceAddresses() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var addresses []string
	for _, i := range interfaces {
		if !ProbeInterface(i) {
			continue
		}
		aa, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range aa {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil {
				return nil, err
			}
			if ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			host := ip.String()
			if ip.IsLinkLocalUnicast() && ip.To4() == nil {
				host += "%" + i.Name
			}
			addresses = append(addresses, host)
		}
	}
	return NormalizeInterfaceAddresses(addresses)
}

// Loopback interfaces can carry link-local addresses that IsLoopback does not
// recognize. They cannot prove LAN exposure or an escape from local-only mode.
func ProbeInterface(i net.Interface) bool {
	return i.Flags&net.FlagUp != 0 && i.Flags&net.FlagLoopback == 0
}

func NormalizeInterfaceAddresses(addresses []string) ([]string, error) {
	set := map[string]bool{}
	for _, address := range addresses {
		host, zone, _ := strings.Cut(address, "%")
		ip := net.ParseIP(host)
		if ip == nil || (zone != "" && (ip.To4() != nil || !ip.IsLinkLocalUnicast())) {
			return nil, fmt.Errorf("invalid interface address")
		}
		if ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		host = ip.String()
		if zone != "" {
			host += "%" + zone
		}
		set[host] = true
	}
	out := make([]string, 0, len(set))
	for host := range set {
		out = append(out, host)
	}
	sort.Strings(out)
	return out, nil
}

func InterfaceAttempts(addresses []string, wildcards, sends bool) []InterfaceAttempt {
	hosts := append([]string(nil), addresses...)
	if wildcards {
		hosts = append(hosts, "0.0.0.0", "::")
	}
	var attempts []InterfaceAttempt
	for _, address := range hosts {
		for _, op := range []string{"bind", "udp-bind"} {
			attempts = append(attempts, InterfaceAttempt{Address: address, Operation: op})
		}
		if sends {
			attempts = append(attempts, InterfaceAttempt{Address: address, Operation: "udp"})
		}
	}
	return attempts
}

// InterfaceAttemptsAtPort shares the same evidence client with selected-port
// proofs. Port zero selects ephemeral binds and UDP destination port 9.
func InterfaceAttemptsAtPort(addresses []string, wildcards, sends bool, port int) []InterfaceAttempt {
	attempts := InterfaceAttempts(addresses, wildcards, sends)
	for i := range attempts {
		attempts[i].Port = port
	}
	return attempts
}

// The host's scoped link-local interface does not exist in a private namespace.
// Test the same address bits on its sole proved interface, lo. A missing host
// scope name is a lookup failure, not evidence that the address cannot bind.
func NamespaceInterfaceAttempts(addresses []string) []InterfaceAttempt {
	attempts := InterfaceAttempts(addresses, false, false)
	for i := range attempts {
		attempts[i].NamespaceScope = strings.Contains(attempts[i].Address, "%")
	}
	return attempts
}

type InterfaceJudgeMode int

const (
	InterfaceLoopbackOnly InterfaceJudgeMode = iota
	InterfaceAllLocal
	InterfacePrivateNamespace
)

// JudgeInterfaceAttempts validates every result and settles escapes first, even
// if another result is missing. EHOSTUNREACH is never an authorization denial.
func JudgeInterfaceAttempts(output string, attempts []InterfaceAttempt, mode InterfaceJudgeMode) ([]InterfaceObservation, Outcome) {
	observed := map[int]int{}
	malformed := false
	escape := false
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "interface-result:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			malformed = true
			continue
		}
		index, e1 := strconv.Atoi(fields[1])
		errno, e2 := strconv.Atoi(fields[2])
		if e1 != nil || e2 != nil || index < 0 || index >= len(attempts) || errno < 0 || errno == 999 {
			malformed = true
			continue
		}
		if _, exists := observed[index]; exists {
			malformed = true
		}
		observed[index] = errno
		unavailable := errno == AddressUnavailableErrno()
		denied := errno == 1 || errno == 13 // EPERM/EACCES
		if mode == InterfaceLoopbackOnly && !denied && !unavailable {
			escape = true
		}
		if mode == InterfacePrivateNamespace && !unavailable {
			if errno == 0 {
				escape = true
			} else {
				malformed = true
			}
		}
	}
	if escape {
		return nil, NotEnforced
	}
	if malformed || len(observed) != len(attempts) || !strings.Contains(output, "interface-canary-ran\n") {
		return nil, Unavailable
	}
	results := make([]InterfaceObservation, 0, len(attempts))
	for i, a := range attempts {
		results = append(results, InterfaceObservation{Address: a.Address, Operation: a.Operation, Errno: observed[i], NamespaceScope: a.NamespaceScope})
	}
	return results, ""
}

// RequireInterfaceExposure catches a Seatbelt change in either direction.
// An address that vanished proves nothing; all available bind attempts must
// match the advertised all-interface exposure. Sends retain their actual errno.
func RequireInterfaceExposure(observations []InterfaceObservation) Outcome {
	for _, o := range observations {
		if o.Operation == "udp" {
			continue
		}
		if o.Errno == AddressUnavailableErrno() {
			continue
		}
		if o.Errno != 0 {
			return ClaimChanged
		}
	}
	return ""
}

func AddressUnavailableErrno() int {
	if runtime.GOOS == "darwin" {
		return 49
	}
	if runtime.GOOS == "windows" {
		return 10049
	}
	return 99
}

func InterfaceExposureDetail(observations []InterfaceObservation, inbound bool) string {
	available := false
	for _, o := range observations {
		ip, _, _ := strings.Cut(o.Address, "%")
		if o.Operation == "bind" && !net.ParseIP(ip).IsUnspecified() && o.Errno == 0 {
			available = true
		}
	}
	if !available {
		return "no interface binds available; wildcard binds tested"
	}
	detail := "binds on all local interfaces: observed allowed"
	if inbound {
		detail += "; host inbound witness reached"
	}
	return detail
}

// Outcome is a structural proof refusal, without provider output.
type Outcome string

const (
	NotEnforced  Outcome = "sandbox_not_enforced"
	Unavailable  Outcome = "sandbox_unavailable"
	ClaimChanged Outcome = "loopback_interface_claim_changed"
)
