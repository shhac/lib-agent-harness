package sandbox

import (
	"net"
	"strings"
	"testing"
)

// Reports must be safe to paste into the task record without host-address
// redaction. Scope names are omitted too; only the address class is retained.
func interfaceAddressClass(address string) string {
	host, _, _ := strings.Cut(address, "%")
	ip := net.ParseIP(host)
	if ip == nil {
		return "invalid"
	}
	family := "v6"
	if ip.To4() != nil {
		family = "v4"
	}
	if ip.IsUnspecified() {
		return "wildcard-" + family
	}
	if ip.IsLoopback() {
		return family + "-loopback"
	}
	if ip.IsLinkLocalUnicast() {
		return family + "-link-local"
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return "v4-cgnat"
	}
	if ip.IsPrivate() {
		if family == "v6" {
			return "v6-ula"
		}
		return "v4-private"
	}
	return family + "-global"
}

func logInterfaceObservation(t *testing.T, o InterfaceObservation) {
	t.Helper()
	t.Logf("class=%s operation=%s errno=%d namespace-scope=%t", interfaceAddressClass(o.Address), o.Operation, o.Errno, o.NamespaceScope)
}

func literalHostRejected(output string) bool {
	return strings.Contains(output, "host must be * or localhost in network address") ||
		strings.Contains(output, "syntax") || strings.Contains(output, "invalid")
}

func TestLoopbackAddressReporting(t *testing.T) {
	for address, want := range map[string]string{
		"10.0.0.1": "v4-private", "100.64.0.1": "v4-cgnat",
		"100.128.0.1": "v4-global", "2001:db8::1": "v6-global",
		"fd00::1": "v6-ula", "fe80::1%scope": "v6-link-local",
		"0.0.0.0": "wildcard-v4", "::": "wildcard-v6",
	} {
		if got := interfaceAddressClass(address); got != want {
			t.Fatalf("address class: got %s want %s", got, want)
		}
	}
	if !literalHostRejected("sandbox-exec: host must be * or localhost in network address") || literalHostRejected("literal-accepted") {
		t.Fatal("literal profile rejection classification changed")
	}
}

func TestLoopbackInterfaceExcluded(t *testing.T) {
	for _, flags := range []net.Flags{0, net.FlagUp | net.FlagLoopback, net.FlagLoopback} {
		if probeInterface(net.Interface{Flags: flags}) {
			t.Fatal("down or loopback interface admitted as escape evidence")
		}
	}
	if !probeInterface(net.Interface{Flags: net.FlagUp}) {
		t.Fatal("up non-loopback interface excluded")
	}
}
