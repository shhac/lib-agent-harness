//go:build !darwin && !linux

package session

import "context"

func proveWorkbench(ctx context.Context, o Options) error { return nil }
func newCommandSandbox(config commandConfig) (*commandSandbox, error) {
	return nil, stateError(StateUnusable)
}
func normalizeWorkbenchSystem(o Options) (Options, error) { return o, nil }
