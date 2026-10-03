//go:build !darwin && !linux

package sandbox

import "context"

func proveWorkbench(ctx context.Context, o Options) (Proof, error) { return Proof{}, platformRefusal() }
func newCommandSandbox(config commandConfig) (*commandSandbox, error) {
	return nil, stateError(StateUnusable)
}
func normalizeWorkbenchSystem(o Options, standalone bool) (Options, error) { return o, nil }
