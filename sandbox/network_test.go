package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func selectedPortRefusalCode() string {
	if runtime.GOOS == "darwin" {
		return RefusedLoopbackPortsUnenforceable
	}
	return RefusedNotOffered
}

func assertPortRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var r *RefusalError
	if !errors.As(err, &r) || r.Code != code || !errors.Is(err, ErrUnsupported) || !strings.Contains(r.Capability.Reason, "Loopback") {
		t.Fatalf("want %s, got %v", code, err)
	}
	if !strings.Contains(r.Capability.Reason, loopbackPortsAlternative) {
		t.Fatalf("refusal does not locate the working alternative: %s", r.Capability.Reason)
	}
	if code == RefusedLoopbackPortsUnenforceable && (r.Capability.Reason != harness.LoopbackPortsSeatbeltReason || r.HarnessFacts().Family != harness.FailureCapability) {
		t.Fatalf("unstable refusal: %+v", r)
	}
}

func TestSelectedPortNetworkValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		o    Options
		code string
	}{
		{"empty", Options{LoopbackPorts: []int{}}, RefusedLimit},
		{"33", Options{LoopbackPorts: make([]int, 33)}, RefusedLimit},
		{"zero-before-conflict", Options{LoopbackPorts: []int{0}}, RefusedLimit},
		{"negative", Options{Loopback: true, LoopbackPorts: []int{-1}}, RefusedLimit},
		{"65536", Options{Loopback: true, LoopbackPorts: []int{65536}}, RefusedLimit},
		{"ports-without-loopback", Options{LoopbackPorts: []int{80}}, RefusedConflict},
		{"control-without-ports", Options{LoopbackControl: "192.0.2.1"}, RefusedConflict},
		{"loopback-control", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "127.0.0.1"}, RefusedConflict},
		{"mapped-loopback", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "::ffff:127.0.0.1"}, RefusedConflict},
		{"scoped-mapped-control", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "::ffff:192.0.2.1%en0"}, RefusedConflict},
		{"v6-loopback", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "::1"}, RefusedConflict},
		{"link-local", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "169.254.1.1"}, RefusedConflict},
		{"scoped-link-local", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "fe80::1%lo0"}, RefusedConflict},
		{"hostname", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "resolver.example"}, RefusedConflict},
		{"multicast", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "224.0.0.1"}, RefusedConflict},
		{"unspecified", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "0.0.0.0"}, RefusedConflict},
		{"valid", Options{Loopback: true, LoopbackPorts: []int{65535, 1, 80}}, selectedPortRefusalCode()},
		{"valid-control", Options{Loopback: true, LoopbackPorts: []int{80}, LoopbackControl: "192.0.2.1"}, selectedPortRefusalCode()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalize(tt.o, true)
			assertPortRefusal(t, err, tt.code)
		})
	}
}

func TestSelectedPortNetworkFreezesInput(t *testing.T) {
	ports := []int{8080, 80, 8080, 1}
	n, err := normalizeNetwork(Options{Loopback: true, LoopbackPorts: ports})
	assertPortRefusal(t, err, selectedPortRefusalCode())
	if !slices.Equal(ports, []int{8080, 80, 8080, 1}) || !slices.Equal(n.LoopbackPorts, []int{1, 80, 8080}) {
		t.Fatalf("normalization: %v / %v", ports, n.LoopbackPorts)
	}
	ports[0] = 7
	if !slices.Equal(n.LoopbackPorts, []int{1, 80, 8080}) {
		t.Fatal("normalized ports alias caller input")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			n, err := normalizeNetwork(Options{Loopback: true, LoopbackPorts: ports})
			assertPortRefusal(t, err, selectedPortRefusalCode())
			if !slices.Equal(n.LoopbackPorts, []int{1, 7, 80, 8080}) {
				t.Errorf("shared normalization state: %v", n.LoopbackPorts)
			}
		})
	}
	wg.Wait()
}

func TestSelectedPortsRefuseBeforeProofAndState(t *testing.T) {
	work, home := t.TempDir(), t.TempDir()
	o := Options{WorkDir: work, RuntimeHome: home, Loopback: true, LoopbackPorts: []int{8080}}
	_, err := openWithProof(context.Background(), o, func(context.Context, Options) (Proof, error) {
		t.Fatal("refusal reached proof")
		return Proof{}, nil
	})
	assertPortRefusal(t, err, selectedPortRefusalCode())
	p, err := Prove(context.Background(), o)
	assertPortRefusal(t, err, selectedPortRefusalCode())
	if p.key != "" || p.request != nil {
		t.Fatal("refusal created proof evidence")
	}
	for _, dir := range []string{work, home} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("refusal changed directory: %v %v", entries, err)
		}
	}
}

func TestNilSelectedPortsNetworkUnchanged(t *testing.T) {
	o := Options{Loopback: true, LoopbackLocalOnly: true, WorkDir: "/work", RuntimeHome: "/runtime"}
	n, err := normalizeNetwork(o)
	if err != nil || n.LoopbackPorts != nil || n.LoopbackControl != "" || n.WorkDir != o.WorkDir || n.RuntimeHome != o.RuntimeHome || n.Loopback != o.Loopback || n.LoopbackLocalOnly != o.LoopbackLocalOnly {
		t.Fatalf("nil-port normalization changed options: %+v %v", n, err)
	}
}

// A refused request must not enter the state sweep, including the permission
// repair used to reclaim scratch left by earlier commands.
func TestSelectedPortsRefuseBeforeReadonlyStateReclamation(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "commands", "leftover", "workbench")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(state, "witness")
	if err := os.WriteFile(witness, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(state, 0700) })
	before, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{WorkDir: t.TempDir(), RuntimeHome: home, Loopback: true, LoopbackPorts: []int{8080}}
	_, err = Open(context.Background(), o)
	assertPortRefusal(t, err, selectedPortRefusalCode())
	_, err = Prove(context.Background(), o)
	assertPortRefusal(t, err, selectedPortRefusalCode())
	after, err := os.Stat(state)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("refusal repaired or replaced existing state: %v", err)
	}
	data, err := os.ReadFile(witness)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("refusal reclaimed existing state: %q %v", data, err)
	}
}
