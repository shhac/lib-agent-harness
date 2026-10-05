//go:build darwin

package session

import (
	"context"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// No nested Seatbelt or network prerequisite: the public refusal never skips.
func TestWorkbenchSelectedPortsRealProof(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, Workbench: &Workbench{Commands: &Commands{Loopback: true, LoopbackPorts: []int{8080}}}}
	_, err := Start(context.Background(), o)
	requireSelectedPortsRefusal(t, err, RefusedLoopbackPortsUnenforceable)
	err = proveWorkbench(context.Background(), o)
	requireSelectedPortsRefusal(t, err, RefusedLoopbackPortsUnenforceable)
}
