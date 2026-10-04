package sandboxprobe

import (
	"fmt"
	"strings"
	"testing"
)

func TestAllLocalInterfaceProtocol(t *testing.T) {
	attempts := InterfaceAttempts([]string{"192.0.2.1"}, false, true)
	good := "interface-result:0:0\ninterface-result:1:0\ninterface-result:2:65\ninterface-canary-ran\n"
	for _, tc := range []struct {
		name, output string
		want         Outcome
	}{
		{"proved", good, ""},
		{"missing index", strings.ReplaceAll(good, "interface-result:1:0\n", ""), Unavailable},
		{"duplicate", good + "interface-result:0:0\n", Unavailable},
		{"garbled", strings.ReplaceAll(good, "1:0", "1:no"), Unavailable},
		{"client failed", strings.ReplaceAll(good, "1:0", "1:999"), Unavailable},
		{"no terminator", strings.ReplaceAll(good, "interface-canary-ran\n", ""), Unavailable},
		{"TCP denied", strings.ReplaceAll(good, "0:0", "0:13"), ClaimChanged},
		{"UDP denied", strings.ReplaceAll(good, "1:0", "1:1"), ClaimChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed, outcome := JudgeInterfaceAttempts(tc.output, attempts, InterfaceAllLocal)
			if outcome == "" {
				outcome = RequireInterfaceExposure(observed)
			}
			if outcome != tc.want {
				t.Fatalf("%s != %s", outcome, tc.want)
			}
			if outcome == "" && observed[2].Errno != 65 {
				t.Fatal("send errno lost")
			}
		})
	}
	out := fmt.Sprintf("interface-result:0:%d\ninterface-result:1:%d\ninterface-result:2:65\ninterface-canary-ran\n", AddressUnavailableErrno(), AddressUnavailableErrno())
	observed, outcome := JudgeInterfaceAttempts(out, attempts, InterfaceAllLocal)
	if outcome != "" || RequireInterfaceExposure(observed) != "" || InterfaceExposureDetail(observed, false) != "no interface binds available; wildcard binds tested" {
		t.Fatal("unavailable addresses claimed exposure")
	}
}
