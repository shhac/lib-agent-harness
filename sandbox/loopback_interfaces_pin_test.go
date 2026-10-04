//go:build darwin || linux

package sandbox

import (
	"os"
	"testing"
)

func TestInterfacePerlCanaryBytesPinned(t *testing.T) {
	want, err := os.ReadFile("testdata/interface-perl-canary.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := interfacePerlCanary([]interfaceAttempt{{Address: "192.0.2.1", Operation: "bind"}, {Address: "fe80::1%en0", Port: 9, Operation: "udp"}})
	if got != string(want) {
		t.Fatal("interface Perl canary bytes changed")
	}
}
