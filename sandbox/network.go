package sandbox

import (
	"net/netip"
	"runtime"
	"slices"

	harness "github.com/shhac/lib-agent-harness"
)

const loopbackPortsAlternative = "use Loopback via a separate sandbox.Open command sandbox on macOS or Linux for servers started inside the command; command sandboxing is unavailable on Windows and other platforms"

// Validate before filesystem or interface discovery on every platform. This
// ports LAH-24's public input contract; enforcement remains unsupported.
func normalizeNetwork(o Options) (Options, error) {
	if o.LoopbackPorts == nil {
		if o.LoopbackControl != "" {
			return o, refusal("loopback_ports", RefusedConflict, "LoopbackControl requires LoopbackPorts; "+loopbackPortsAlternative)
		}
		return o, nil
	}
	if len(o.LoopbackPorts) == 0 || len(o.LoopbackPorts) > 32 {
		return o, refusal("loopback_ports", RefusedLimit, "LoopbackPorts must contain between 1 and 32 ports; "+loopbackPortsAlternative)
	}
	for _, port := range o.LoopbackPorts {
		if port < 1 || port > 65535 {
			return o, refusal("loopback_ports", RefusedLimit, "loopback ports must be between 1 and 65535; "+loopbackPortsAlternative)
		}
	}
	if !o.Loopback {
		return o, refusal("loopback_ports", RefusedConflict, "LoopbackPorts requires Loopback; "+loopbackPortsAlternative)
	}
	o.LoopbackPorts = slices.Clone(o.LoopbackPorts)
	slices.Sort(o.LoopbackPorts)
	o.LoopbackPorts = slices.Compact(o.LoopbackPorts)
	if o.LoopbackControl != "" {
		addr, err := netip.ParseAddr(o.LoopbackControl)
		zone := addr.Zone()
		addr = addr.Unmap()
		if err != nil || zone != "" || !addr.IsGlobalUnicast() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
			return o, refusal("loopback_control", RefusedConflict, "LoopbackControl must be an off-machine IP literal; "+loopbackPortsAlternative)
		}
		// No interface enumeration: requests are refused regardless of whether
		// this otherwise valid literal belongs to this machine.
		o.LoopbackControl = addr.String()
	}
	code := RefusedNotOffered
	if runtime.GOOS == "darwin" {
		code = RefusedLoopbackPortsUnenforceable
	}
	return o, refusal("loopback_ports", code, harness.Support(harness.OpenAICompatible, harness.Session, harness.LoopbackPorts).Reason)
}
