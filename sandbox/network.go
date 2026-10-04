package sandbox

import (
	"runtime"
	"slices"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

type networkControl = sandboxprobe.ControlTarget

// Validate before filesystem discovery or state creation, on every platform.
func normalizeNetwork(o Options) (Options, error) {
	if o.LoopbackPorts == nil {
		if o.LoopbackControl != "" {
			return o, refusal("loopback_ports", RefusedConflict, "LoopbackControl requires LoopbackPorts")
		}
		return o, nil
	}
	if !o.Loopback {
		return o, refusal("loopback_ports", RefusedConflict, "LoopbackPorts requires Loopback")
	}
	if len(o.LoopbackPorts) == 0 || len(o.LoopbackPorts) > 32 {
		return o, refusal("loopback_ports", RefusedLimit, "LoopbackPorts must contain between 1 and 32 ports")
	}
	for _, port := range o.LoopbackPorts {
		if port < 1 || port > 65535 {
			return o, refusal("loopback_ports", RefusedLimit, "loopback ports must be between 1 and 65535")
		}
	}
	o.LoopbackPorts = slices.Clone(o.LoopbackPorts)
	slices.Sort(o.LoopbackPorts)
	o.LoopbackPorts = slices.Compact(o.LoopbackPorts)
	if o.LoopbackControl != "" {
		addr, reason := sandboxprobe.ValidateControl(o.LoopbackControl)
		if reason != "" {
			return o, refusal("loopback_control", RefusedConflict, reason)
		}
		o.LoopbackControl = addr
	}
	if runtime.GOOS != "darwin" {
		return o, &RefusalError{Operation: "loopback_ports", Code: RefusedNotOffered, Capability: harness.Support(harness.OpenAICompatible, harness.Session, harness.LoopbackPorts)}
	}
	return o, nil
}
