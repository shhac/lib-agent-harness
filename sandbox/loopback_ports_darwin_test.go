package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestSelectedPortDenialsExcludeWildcardReach(t *testing.T) {
	previous := interfaceAddresses
	t.Cleanup(func() { interfaceAddresses = previous })
	for _, addresses := range [][]string{nil, {"192.0.2.100", "2001:db8::100", "fe80::123%fixture0"}} {
		interfaceAddresses = func() ([]string, error) { return addresses, nil }
		attempts, err := portDenials([]int{4000}, 3000, "192.0.2.1", "192.0.2.53")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, attempt := range attempts {
			host, _, _ := strings.Cut(attempt.Addr, "%")
			if attempt.Kind == "reach" && net.ParseIP(host).IsUnspecified() {
				t.Fatalf("wildcard reach would falsely report loopback escape: %+v", attempt)
			}
			if attempt.Kind == "reach" && attempt.Port == 3000 {
				seen[attempt.Addr] = true
			}
		}
		if len(seen) != len(addresses) {
			t.Fatalf("lost interface reach attempts: %v", seen)
		}
		for _, address := range addresses {
			if !seen[address] {
				t.Fatalf("missing interface reach for %s", address)
			}
		}
		// Filtering reaches must not remove wildcard TCP/UDP bind controls.
		binds := map[string]bool{}
		for _, attempt := range interfaceAttemptsAtPort(addresses, true, true, 3000) {
			if net.ParseIP(attempt.Address).IsUnspecified() && (attempt.Operation == "bind" || attempt.Operation == "udp-bind") {
				binds[attempt.Address+"/"+attempt.Operation] = true
			}
		}
		if len(binds) != 4 {
			t.Fatalf("wildcard bind coverage lost: %v", binds)
		}
	}
}

func TestSelectedPortProfileAndProofKeys(t *testing.T) {
	l := workbenchLayout{Work: "/workspace", Home: "/home", Tmp: "/tmp", System: workbenchSystemDirs(), Loopback: true}
	old := seatbeltProfile(l)
	if !strings.Contains(old, `localhost:*`) {
		t.Fatal("legacy loopback changed")
	}
	l.LoopbackPorts = []int{3000, 8080}
	got := seatbeltProfile(l)
	if strings.Contains(got, "localhost:*") {
		t.Fatal("selected ports widened")
	}
	for _, port := range l.LoopbackPorts {
		for _, op := range []string{"network-bind (local", "network-inbound (local", "network-outbound (remote"} {
			if !strings.Contains(got, fmt.Sprintf("(allow %s ip \"localhost:%d\"))", op, port)) {
				t.Fatal("missing per-port selector")
			}
		}
	}
	o := Options{WorkDir: "/workspace", RuntimeHome: "/runtime", Loopback: true, Env: []string{"LANG=C"}}
	template := sha256.Sum256([]byte(seatbeltProfile(workbenchLayout{Work: "/workspace", Home: "/home", Tmp: "/tmp", Read: []string{"/read"}, System: workbenchSystemDirs(), Write: true, Loopback: true})))
	want, err := legacyworkbenchProbeKey(o, workbenchSystemDirs(), workbenchSeatbeltVersion+":"+workbenchPathVersion+":"+hex.EncodeToString(template[:]))
	if err != nil {
		t.Fatal(err)
	}
	key, err := workbenchProbeKey(o, workbenchSystemDirs())
	if err != nil || key != want {
		t.Fatal("absent policy changed key", err)
	}
	keys := map[string]bool{key: true}
	for _, policy := range []struct {
		ports   []int
		control string
	}{{[]int{3000}, ""}, {[]int{8080}, ""}, {[]int{3000}, "192.0.2.53"}, {[]int{3000}, "192.0.2.54"}} {
		o.LoopbackPorts, o.LoopbackControl = policy.ports, policy.control
		next, err := workbenchProbeKey(o, workbenchSystemDirs())
		if err != nil || keys[next] {
			t.Fatalf("proof-key collision: %v", err)
		}
		keys[next] = true
	}
}

func TestSelectedPortJudgeEveryMissingPositiveAndEscape(t *testing.T) {
	attempts := []portAttempt{{"positive-bind", "127.0.0.1", 3000, "bind"}, {"positive-reach", "::1", 8080, "reach"}, {"reach", "127.0.0.1", 8340, "8340"}, {"bind", "::1", 4000, "unselected-ipv6"}, {"bind", "0.0.0.0", 3000, "wildcard"}, {"reach", "192.0.2.100", 3000, "interface"}, {"reach", "192.0.2.53", 53, "tcp53"}, {"udp", "192.0.2.53", 53, "udp53"}, {"reach", "192.0.2.1", 443, "tcp443"}}
	good := "ports-ran\ncanary-ran\ninbound\nreceipt\n"
	var markers []string
	for _, a := range attempts {
		prefix := "denied-"
		if strings.HasPrefix(a.Kind, "positive") {
			prefix = "positive-"
		}
		markers = append(markers, prefix+a.Label)
		good += prefix + a.Label + "\n"
	}
	if err := judgePortCanary(good, attempts, true); err != nil {
		t.Fatal(err)
	}
	markers = append(markers, "ports-ran", "canary-ran", "inbound", "receipt")
	for _, marker := range markers {
		t.Run("missing/"+marker, func(t *testing.T) {
			err := judgePortCanary(strings.ReplaceAll(good, marker+"\n", ""), attempts, true)
			var proof *ProofError
			if !errors.As(err, &proof) || proof.Code != CapabilitySandboxUnavailable {
				t.Fatalf("missing evidence accepted: %v", err)
			}
		})
	}
	for _, a := range attempts {
		t.Run("escape/"+a.Label, func(t *testing.T) {
			err := judgePortCanary(good+"escape-"+a.Label+"\n", attempts, true)
			var proof *ProofError
			if !errors.As(err, &proof) || proof.Code != CapabilitySandboxNotEnforced {
				t.Fatal(err)
			}
		})
	}
	for _, output := range []string{"", "unavailable-tcp53\nports-ran\ncanary-ran\n", "denied-tcp53\n"} {
		if judgePortCanary(output, attempts, true) == nil {
			t.Fatal("partial output accepted")
		}
	}
}

func TestSelectedPortRealSeatbelt(t *testing.T) {
	o := Options{Loopback: true, LoopbackPorts: []int{8080}}
	_, err := Open(context.Background(), o)
	assertPortRefusal(t, err, RefusedLoopbackPortsUnenforceable)
	_, err = Prove(context.Background(), o)
	assertPortRefusal(t, err, RefusedLoopbackPortsUnenforceable)
}
