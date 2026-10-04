package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

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

func TestSelectedPortProofPublicationAndFacts(t *testing.T) {
	previous := verified
	verified = &verificationCache{seen: map[string]bool{}}
	defer func() { verified = previous }()
	o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Loopback: true, LoopbackPorts: []int{3000}}
	baseRuns, portRuns := 0, 0
	base := func(_ context.Context, n Options, _ []string) error {
		baseRuns++
		if n.LoopbackPorts != nil || n.LoopbackControl != "" {
			t.Fatal("changed legacy execution proof")
		}
		return nil
	}
	target := networkControl{Addr: "192.0.2.53", Source: "configured"}
	for _, code := range []string{CapabilitySandboxUnavailable, CapabilitySandboxNotEnforced, CapabilityProbeTimeout} {
		_, err := proveWorkbenchStages(t.Context(), o, base, func(context.Context, Options, []string) (networkControl, error) {
			return networkControl{}, &ProofError{Code: code, Step: ProofStepNetwork}
		})
		var failure *ProofError
		if !errors.As(err, &failure) || failure.Code != code {
			t.Fatal(err)
		}
		if len(verified.seen) != 0 {
			t.Fatal("failed stage published evidence")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := proveWorkbenchStages(ctx, o, base, func(context.Context, Options, []string) (networkControl, error) { cancel(); return target, nil })
	if err == nil || len(verified.seen) != 0 {
		t.Fatal("cancelled stage published evidence")
	}
	var cancelled *ProofError
	if !errors.As(err, &cancelled) || cancelled.HarnessFacts().NetworkControlAddr != target.Addr || cancelled.HarnessFacts().NetworkControlSource != target.Source {
		t.Fatal("cancelled proof lost chosen control", err)
	}
	ports := func(_ context.Context, n Options, _ []string) (networkControl, error) {
		portRuns++
		if n.LoopbackPorts == nil {
			t.Fatal("selected stage lost policy")
		}
		return target, nil
	}
	p, err := proveWorkbenchStages(t.Context(), o, base, ports)
	if err != nil {
		t.Fatal(err)
	}
	address, source := p.NetworkControl()
	if address != target.Addr || source != target.Source {
		t.Fatal("control facts lost")
	}
	before := baseRuns
	p, err = proveWorkbenchStages(t.Context(), o, base, ports)
	if err != nil || portRuns != 1 || baseRuns != before {
		t.Fatal("matching evidence not reused", err)
	}
	address, source = p.NetworkControl()
	if address != target.Addr || source != target.Source {
		t.Fatal("cached control facts lost")
	}
	o.LoopbackPorts = []int{8080}
	if _, err := proveWorkbenchStages(t.Context(), o, base, ports); err != nil {
		t.Fatal(err)
	}
	o.LoopbackControl = "192.0.2.54"
	if _, err := proveWorkbenchStages(t.Context(), o, base, ports); err != nil {
		t.Fatal(err)
	}
	if portRuns != 3 {
		t.Fatal("incompatible port/control policy reused evidence")
	}
}

func TestSelectedPortRealSeatbelt(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	testenv.RequireLoopback(t)
	opts := commandSandboxOptions(t, false)
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	opts.Loopback = true
	opts.LoopbackPorts = []int{port}
	opts.LoopbackControl = os.Getenv("AGENT_HARNESS_TEST_LOOPBACK_CONTROL")
	ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
	defer cancel()
	proof, err := Prove(ctx, opts)
	if err != nil {
		t.Fatalf("selected-port Seatbelt proof failed: %v", err)
	}
	address, source := proof.NetworkControl()
	if address == "" || (source != "caller" && source != "configured") {
		t.Fatalf("missing control facts: %q %q", address, source)
	}
	t.Logf("Seatbelt selected-port stages A/B proved; DNS control %s (%s)", address, source)
	// Open shares that proof, freezes the caller slice, and launches a server.
	s, err := Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	opts.LoopbackPorts[0] = port + 1
	h, err := s.Start(ctx, CommandRequest{Command: fmt.Sprintf("printf 'selected-server\\n' | nc -l 127.0.0.1 %d", port)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	var c net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("selected server not reachable", err)
	}
	c.Close()
	excluded, e := unselectedPort([]int{port})
	if e != nil {
		t.Fatal(e)
	}
	attempts, e := portDenials([]int{excluded}, port, address, address)
	if e != nil {
		t.Fatal(e)
	}
	// Direct command result, independent of the pre-launch judge.
	result, err := s.Run(ctx, CommandRequest{Command: portScript(attempts) + "echo canary-ran"})
	if err != nil || judgePortCanary(result.Stdout, attempts, false) != nil {
		t.Fatalf("command widened ports: %+v %v", result, err)
	}
}
