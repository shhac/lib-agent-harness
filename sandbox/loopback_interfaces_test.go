package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestInterfaceAttemptJudgment(t *testing.T) {
	attempts := interfaceAttempts([]string{"192.0.2.1"}, false, true)
	for _, tc := range []struct {
		name, output, code string
		mode               interfaceJudgeMode
	}{
		{"strict escape", "interface-result:0:0\n", CapabilitySandboxNotEnforced, interfaceLoopbackOnly},
		{"namespace escape", "interface-result:0:0\n", CapabilitySandboxNotEnforced, interfacePrivateNamespace},
		{"namespace denial is incomplete evidence", "interface-result:0:13\n", CapabilitySandboxUnavailable, interfacePrivateNamespace},
		{"host unreachable is not denied", "interface-result:2:65\n", CapabilitySandboxNotEnforced, interfaceLoopbackOnly},
		{"unavailable is incomplete", fmt.Sprintf("interface-result:0:%d\n", addressUnavailableErrno()), CapabilitySandboxUnavailable, interfaceLoopbackOnly},
		{"missing", "interface-canary-ran\n", CapabilitySandboxUnavailable, interfaceAllLocal},
		{"garbled", "interface-result:0:no\n", CapabilitySandboxUnavailable, interfaceAllLocal},
		{"missing client", "interface-result:0:999\n", CapabilitySandboxUnavailable, interfaceAllLocal},
		{"all local", "interface-result:0:0\ninterface-result:1:0\ninterface-result:2:65\ninterface-canary-ran\n", "", interfaceAllLocal},
		{"denied", "interface-result:0:1\ninterface-result:1:13\ninterface-result:2:1\ninterface-canary-ran\n", "", interfaceLoopbackOnly},
		{"duplicate", "interface-result:0:1\ninterface-result:0:1\ninterface-result:1:13\ninterface-result:2:1\ninterface-canary-ran\n", CapabilitySandboxUnavailable, interfaceLoopbackOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed, err := judgeInterfaceAttempts(tc.output, attempts, tc.mode)
			if tc.code == "" {
				if err != nil || len(observed) != len(attempts) {
					t.Fatalf("%+v %v", observed, err)
				}
				return
			}
			var p *ProofError
			if !errors.As(err, &p) || p.Code != tc.code {
				t.Fatalf("want %s: %v", tc.code, err)
			}
		})
	}
	binds := interfaceAttempts([]string{"192.0.2.1"}, false, false)
	if _, err := judgeInterfaceAttempts(fmt.Sprintf("interface-result:0:%d\ninterface-result:1:%d\ninterface-canary-ran\n", addressUnavailableErrno(), addressUnavailableErrno()), binds, interfacePrivateNamespace); err != nil {
		t.Fatal(err)
	}
	if _, err := judgeInterfaceAttempts("interface-canary-ran\n", nil, interfaceAllLocal); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(localOnlyDetail(Options{LoopbackLocalOnly: true}, nil), "no interface addresses") {
		t.Fatal("empty enumeration claimed interface evidence")
	}
}

func TestInterfaceAddressAttempts(t *testing.T) {
	addresses, err := normalizeInterfaceAddresses([]string{"127.0.0.1", "::1", "192.0.2.1", "192.0.2.1", "fe80::1%en0", "0.0.0.0"})
	if err != nil || !reflect.DeepEqual(addresses, []string{"192.0.2.1", "fe80::1%en0"}) {
		t.Fatalf("%v %v", addresses, err)
	}
	attempts := interfaceAttempts(addresses, true, true)
	if len(attempts) != 12 || attempts[6].Address != "0.0.0.0" || attempts[9].Address != "::" {
		t.Fatalf("%+v", attempts)
	}
	if _, err := normalizeInterfaceAddresses([]string{"garbage"}); err == nil {
		t.Fatal("garbage accepted")
	}
	for _, a := range interfaceAttemptsAtPort(addresses, true, true, 57648) {
		if a.Port != 57648 {
			t.Fatalf("selected-port helper lost port: %+v", a)
		}
	}
	for _, a := range namespaceInterfaceAttempts(addresses) {
		if a.NamespaceScope != strings.Contains(a.Address, "%") {
			t.Fatalf("wrong namespace scope: %+v", a)
		}
	}
}

func TestLoopbackLocalOnlyRefusesBeforeProof(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		if loopback && runtime.GOOS == "linux" {
			continue
		}
		called := false
		_, err := openWithProof(context.Background(), Options{Loopback: loopback, LoopbackLocalOnly: true}, func(context.Context, Options) (Proof, error) { called = true; return Proof{}, nil })
		var refused *RefusalError
		if called || !errors.As(err, &refused) {
			t.Fatalf("called=%t err=%v", called, err)
		}
		want := RefusedConflict
		if loopback {
			want = RefusedNotOffered
			if runtime.GOOS == "darwin" {
				want = RefusedLoopbackNotLocal
			}
		}
		if refused.Code != want {
			t.Fatalf("%s != %s", refused.Code, want)
		}
	}
}

func TestInterfaceExposureClaim(t *testing.T) {
	for _, errno := range []int{0, addressUnavailableErrno(), 1, 13, 65} {
		err := requireInterfaceExposure([]InterfaceObservation{{Address: "192.0.2.1", Operation: "bind", Errno: errno}})
		if (err == nil) != (errno == 0 || errno == addressUnavailableErrno()) {
			t.Fatalf("errno=%d err=%v", errno, err)
		}
	}
}

func TestInterfacePlatformErrnos(t *testing.T) {
	wrong := 49
	if runtime.GOOS == "darwin" {
		wrong = 99
	}
	attempts := interfaceAttempts([]string{"192.0.2.1"}, false, false)
	_, err := judgeInterfaceAttempts(fmt.Sprintf("interface-result:0:%d\n", wrong), attempts, interfaceLoopbackOnly)
	var p *ProofError
	if !errors.As(err, &p) || p.Code != CapabilitySandboxNotEnforced {
		t.Fatalf("foreign errno treated as unavailable: %v", err)
	}
	err = requireInterfaceExposure([]InterfaceObservation{{Operation: "bind", Errno: 13}})
	if !errors.As(err, &p) || p.Code != CapabilityLoopbackClaimChanged || !strings.Contains(err.Error(), "re-audit") {
		t.Fatalf("unnamed claim change: %v", err)
	}
}

func TestInterfaceWitnessRetriesUnavailableAddress(t *testing.T) {
	attempts := interfaceAttempts([]string{"192.0.2.1", "192.0.2.2"}, true, true)
	calls := 0
	host, _, err := listenInterfaceWitness(attempts, func(network, address string) (net.Listener, error) {
		calls++
		if address == "192.0.2.1:0" {
			return nil, fmt.Errorf("address vanished")
		}
		if address != "192.0.2.2:0" || network != "tcp" {
			t.Fatalf("unexpected witness: %s %s", network, address)
		}
		return nil, nil
	})
	if err != nil || host != "192.0.2.2" || calls != 2 {
		t.Fatalf("%s %d %v", host, calls, err)
	}
	_, _, err = listenInterfaceWitness(attempts, func(string, string) (net.Listener, error) { return nil, fmt.Errorf("unavailable") })
	if err == nil {
		t.Fatal("missing witness accepted")
	}
}

func TestUnavailableInterfacesDoNotClaimExposure(t *testing.T) {
	observations := []InterfaceObservation{{Address: "192.0.2.1", Operation: "bind", Errno: addressUnavailableErrno()}, {Address: "0.0.0.0", Operation: "bind", Errno: 0}}
	if detail := interfaceExposureDetail(observations, false); strings.Contains(detail, "observed allowed") || !strings.Contains(detail, "no interface binds available") {
		t.Fatal(detail)
	}
}
