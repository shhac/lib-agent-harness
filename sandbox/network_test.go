package sandbox

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestNetworkPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options Options
		code    string
	}{
		{"absent", Options{}, ""},
		{"control absent ports", Options{LoopbackControl: "192.0.2.1"}, RefusedConflict},
		{"missing loopback", Options{LoopbackPorts: []int{80}}, RefusedConflict},
		{"empty", Options{Loopback: true, LoopbackPorts: []int{}}, RefusedLimit},
		{"zero", Options{Loopback: true, LoopbackPorts: []int{0}}, RefusedLimit},
		{"negative", Options{Loopback: true, LoopbackPorts: []int{-1}}, RefusedLimit},
		{"overflow", Options{Loopback: true, LoopbackPorts: []int{65536}}, RefusedLimit},
		{"cap", Options{Loopback: true, LoopbackPorts: make([]int, 33)}, RefusedLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeNetwork(tc.options)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var refused *RefusalError
			if !errors.As(err, &refused) || refused.Code != tc.code {
				t.Fatalf("%s: %v", tc.code, err)
			}
		})
	}
	for _, control := range []string{"garbage", "127.0.0.1", "::1", "::ffff:127.0.0.1", "0.0.0.0", "::", "169.254.1.1", "fe80::1", "224.0.0.1", "ff02::1"} {
		_, err := normalizeNetwork(Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: control})
		var refused *RefusalError
		if !errors.As(err, &refused) || refused.Code != RefusedConflict {
			t.Fatalf("control %q: %v", control, err)
		}
	}
	ports := []int{65535, 1, 80, 80}
	n, err := normalizeNetwork(Options{Loopback: true, LoopbackPorts: ports})
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		var refused *RefusalError
		if !errors.As(err, &refused) || refused.Code != RefusedNotOffered || refused.Capability != harness.Support(harness.OpenAICompatible, harness.Session, harness.LoopbackPorts) {
			t.Fatal(err)
		}
	}
	ports[0] = 42
	if !slices.Equal(n.LoopbackPorts, []int{1, 80, 65535}) {
		t.Fatalf("policy not frozen: %v", n.LoopbackPorts)
	}
}

func TestSelectedPortRefusalBeforeState(t *testing.T) {
	for _, ports := range [][]int{{}, {0}, {80}} {
		if runtime.GOOS == "darwin" && len(ports) == 1 && ports[0] == 80 {
			continue
		}
		o := Options{Loopback: true, LoopbackPorts: ports, RuntimeHome: "must-not-be-created"}
		_, err := Open(context.Background(), o)
		_, proofErr := Prove(context.Background(), o)
		var a, b *RefusalError
		if !errors.As(err, &a) || !errors.As(proofErr, &b) || a.Code != b.Code || a.Capability != b.Capability {
			t.Fatalf("entry-point mismatch: %v / %v", err, proofErr)
		}
	}
}

func TestSelectedPortControlCacheIsolation(t *testing.T) {
	c := &verificationCache{seen: map[string]bool{}}
	target := networkControl{Addr: "192.0.2.1", Source: "caller"}
	c.recordControl("ports-a-control-a", target)
	if got, ok := c.control("ports-a-control-a"); !ok || got != target {
		t.Fatal("lost evidence facts")
	}
	for _, key := range []string{"ports-b-control-a", "ports-a-control-b"} {
		if _, ok := c.control(key); ok {
			t.Fatal("reused incompatible evidence")
		}
	}
	c.record("legacy")
	if _, ok := c.control("legacy"); ok {
		t.Fatal("legacy proof certified selected ports")
	}
}
