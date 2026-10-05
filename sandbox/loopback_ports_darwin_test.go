//go:build darwin

package sandbox

import (
	"context"
	"testing"
)

// Public refusal needs no Seatbelt prerequisite and must never skip, including
// under NO_SKIP or an outer sandbox. The kept escape canary belongs to LAH-43.
func TestSelectedPortRealSeatbelt(t *testing.T) {
	o := Options{Loopback: true, LoopbackPorts: []int{8080}}
	_, err := Open(context.Background(), o)
	assertPortRefusal(t, err, RefusedLoopbackPortsUnenforceable)
	_, err = Prove(context.Background(), o)
	assertPortRefusal(t, err, RefusedLoopbackPortsUnenforceable)
}
