package session

import (
	"errors"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/sandbox"
)

// Selected-port requests are refused before paths, logins, discovery or launch.
// Reuse sandbox normalization so validation order and codes stay identical.
func refuseSelectedPorts(o Options) error {
	var requests []sandbox.Options
	if s := o.Sandbox; s != nil && (s.LoopbackPorts != nil || s.LoopbackControl != "") {
		requests = append(requests, sandbox.Options{Loopback: s.Loopback, LoopbackPorts: s.LoopbackPorts, LoopbackControl: s.LoopbackControl})
	}
	if w := o.Workbench; w != nil && w.Commands != nil && (w.Commands.LoopbackPorts != nil || w.Commands.LoopbackControl != "") {
		requests = append(requests, commandOptions(o))
	}
	for _, request := range requests {
		_, err := sandboxbridge.NormalizeWorkbench(request)
		var r *sandbox.RefusalError
		if errors.As(err, &r) && (r.Code == sandbox.RefusedNotOffered || r.Code == sandbox.RefusedLoopbackPortsUnenforceable) {
			// Validation uses the common contract; capability reasons name the
			// actual session engine, including native yes/no binding controls.
			r.Capability = harness.Support(o.Provider.Engine, harness.Session, harness.LoopbackPorts)
		}
		if err != nil {
			return fromSandbox(err, o)
		}
	}
	return nil
}
