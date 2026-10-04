package sandboxprobe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	NoOffMachineResolver        = "no_off_machine_resolver"
	ControlOnThisMachine        = "control_on_this_machine"
	ControlInvalidIP            = "control_invalid_ip"
	ControlInterfaceUnavailable = "control_interface_unavailable"
	UDPUnanswered               = "udp_unanswered"
	TCPUnanswered               = "tcp_unanswered"
	ControlDeadline             = "control_deadline"
	PortClientUnavailable       = "port_client_unavailable"
	IPv6ControlUnavailable      = "ipv6_control_unavailable"
)

// ControlTarget records destination metadata only, never DNS payloads.
type ControlTarget struct{ Addr, Source string }

// SanitizeControl permits only canonical IP literals and known source labels.
// Fact extraction does not rediscover interfaces or perform network operations.
func SanitizeControl(target ControlTarget) ControlTarget {
	switch target.Source {
	case "caller", "configured", "systemd_upstream":
	default:
		return ControlTarget{}
	}
	addr, reason := validateControl(target.Addr, nil)
	if reason != "" {
		return ControlTarget{}
	}
	return ControlTarget{Addr: addr, Source: target.Source}
}

// ValidateControl accepts private off-machine resolvers, but never addresses
// on this machine. Failure to enumerate interfaces is a refusal, not evidence.
func ValidateControl(value string) (string, string) {
	if _, reason := validateControl(value, nil); reason != "" {
		return "", reason
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", ControlInterfaceUnavailable
	}
	local := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil {
			local = append(local, prefix.Addr().Unmap())
		}
	}
	return validateControl(value, local)
}

func validateControl(value string, local []netip.Addr) (string, string) {
	addr, err := netip.ParseAddr(value)
	if err != nil || addr.Zone() != "" {
		return "", ControlInvalidIP
	}
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return "", ControlOnThisMachine
	}
	for _, a := range local {
		if a.Unmap() == addr {
			return "", ControlOnThisMachine
		}
	}
	return addr.String(), ""
}

// ResolverAddresses parses resolv.conf without resolving names or contacting DNS.
func ResolverAddresses(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, fields[1])
		}
	}
	return out
}

// ScutilAddresses parses nameserver[n] entries, including scoped resolvers.
func ScutilAddresses(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && strings.HasPrefix(fields[0], "nameserver[") && fields[1] == ":" {
			out = append(out, fields[2])
		}
	}
	return out
}

// selectControl is pure so resolver ordering and local-stub handling are
// testable on every OS. An explicit caller address never falls back.
func selectControl(caller string, configured, upstream []string, local []netip.Addr) (ControlTarget, string) {
	targets, reason := selectControls(caller, configured, upstream, local)
	if len(targets) == 0 {
		return ControlTarget{}, reason
	}
	return targets[0], reason
}

func selectControls(caller string, configured, upstream []string, local []netip.Addr) ([]ControlTarget, string) {
	if caller != "" {
		addr, reason := validateControl(caller, local)
		return []ControlTarget{{addr, "caller"}}, reason
	}
	var targets []ControlTarget
	seen := map[string]bool{}
	for _, group := range []struct {
		source string
		addrs  []string
	}{{"configured", configured}, {"systemd_upstream", upstream}} {
		for _, value := range group.addrs {
			if addr, reason := validateControl(value, local); reason == "" && !seen[addr] {
				seen[addr] = true
				targets = append(targets, ControlTarget{addr, group.source})
			}
		}
	}
	if len(targets) == 0 {
		return nil, NoOffMachineResolver
	}
	return targets, ""
}

// DiscoverControl never selects a fixed public resolver. Linux's upstream file
// is consulted only when the configured file contains exclusively local stubs.
func DiscoverControl(ctx context.Context, caller string) (ControlTarget, string) {
	targets, reason := discoverControls(ctx, caller)
	if len(targets) == 0 {
		return ControlTarget{}, reason
	}
	return targets[0], reason
}

// DiscoverAndProveControl tries configured candidates within one short deadline.
// An explicit caller destination is the only candidate and never falls back.
func DiscoverAndProveControl(ctx context.Context, caller string) (ControlTarget, string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	targets, reason := discoverControls(ctx, caller)
	if reason != "" {
		if len(targets) > 0 {
			return targets[0], reason
		}
		return ControlTarget{}, reason
	}
	return proveControlCandidates(ctx, targets, ProveDNSControl)
}

func proveControlCandidates(ctx context.Context, targets []ControlTarget, prove func(context.Context, ControlTarget) string) (ControlTarget, string) {
	var target ControlTarget
	reason := NoOffMachineResolver
	for i, t := range targets {
		if ctx.Err() != nil {
			return target, ControlDeadline
		}
		target = t
		deadline, _ := ctx.Deadline()
		budget := time.Until(deadline) / time.Duration(len(targets)-i)
		attempt, cancel := context.WithTimeout(ctx, budget)
		reason = prove(attempt, target)
		cancel()
		if ctx.Err() != nil {
			return target, ControlDeadline
		}
		if reason == "" {
			return target, ""
		}
	}
	return target, reason
}

func discoverControls(ctx context.Context, caller string) ([]ControlTarget, string) {
	if ctx.Err() != nil {
		return nil, ControlDeadline
	}
	if caller != "" {
		addr, reason := ValidateControl(caller)
		return []ControlTarget{{addr, "caller"}}, reason
	}
	var configured, upstream []string
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns")
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if ctx.Err() != nil {
			return nil, ControlDeadline
		}
		if err == nil && len(out) <= 256<<10 {
			configured = ScutilAddresses(string(out))
		}
	case "linux":
		out, _ := os.ReadFile("/etc/resolv.conf")
		configured = ResolverAddresses(string(out))
		stubsOnly := len(configured) > 0
		for _, value := range configured {
			a, err := netip.ParseAddr(value)
			if err != nil || !a.Unmap().IsLoopback() {
				stubsOnly = false
			}
		}
		if stubsOnly {
			out, _ = os.ReadFile("/run/systemd/resolve/resolv.conf")
			upstream = ResolverAddresses(string(out))
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, ControlInterfaceUnavailable
	}
	var local []netip.Addr
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			local = append(local, p.Addr())
		}
	}
	return selectControls("", configured, upstream, local)
}

// ProveDNSControl requires a matching UDP DNS reply and a TCP connection to
// port 53, within one short deadline. Errors contain only fixed reasons.
func ProveDNSControl(ctx context.Context, target ControlTarget) string {
	addr, reason := ValidateControl(target.Addr)
	if reason != "" {
		return reason
	}
	return proveDNSControl(ctx, net.JoinHostPort(addr, "53"), (&net.Dialer{}).DialContext)
}

type controlDial func(context.Context, string, string) (net.Conn, error)

// The injectable dialer is private and permits loopback fakes only in tests.
func proveDNSControl(ctx context.Context, address string, dial controlDial) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	failure := func(reason string) string {
		deadline, _ := ctx.Deadline()
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return ControlDeadline
		}
		return reason
	}
	// Root NS, no recursion and no caller data. Random transaction ID only.
	query := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 1}
	if _, err := rand.Read(query[:2]); err != nil {
		return UDPUnanswered
	}
	conn, err := dial(ctx, "udp", address)
	if err != nil {
		return failure(UDPUnanswered)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	if _, err = conn.Write(query); err != nil {
		return failure(UDPUnanswered)
	}
	reply := make([]byte, 4096)
	n, err := conn.Read(reply)
	if err != nil {
		return failure(UDPUnanswered)
	}
	if n < 12 || binary.BigEndian.Uint16(reply[:2]) != binary.BigEndian.Uint16(query[:2]) || reply[2]&0x80 == 0 {
		return UDPUnanswered
	}
	tcp, err := dial(ctx, "tcp", address)
	if err != nil {
		return failure(TCPUnanswered)
	}
	tcp.Close()
	if ctx.Err() != nil {
		return ControlDeadline
	}
	return ""
}
