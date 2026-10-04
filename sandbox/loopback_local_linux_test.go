//go:build linux

package sandbox

import (
	"context"
	"testing"
)

func TestStandaloneLoopbackLocalOnlyRealProof(t *testing.T) {
	requireCommandPlatform(t)
	o := commandSandboxOptions(t, true)
	o.LoopbackLocalOnly = true
	p, err := Prove(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(p.NetworkDetail())
	if p.NetworkDetail() == "" {
		t.Fatal("missing local-only evidence")
	}
	for _, observation := range p.NetworkObservations() {
		logInterfaceObservation(t, observation)
		if observation.Errno != 99 {
			t.Fatalf("host interface bind not unavailable: class=%s operation=%s errno=%d", interfaceAddressClass(observation.Address), observation.Operation, observation.Errno)
		}
	}
	s := openTestCommandSandbox(t, o)
	r, err := s.Run(context.Background(), CommandRequest{Command: "echo private-loopback-proved"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
