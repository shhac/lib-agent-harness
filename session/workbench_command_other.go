//go:build !darwin && !linux

package session

import "context"

func proveWorkbench(ctx context.Context, o Options) error             { return nil }
func setupWorkbenchCommands(w *workspace, o Options, id string) error { return nil }
func normalizeWorkbenchSystem(o Options) (Options, error)             { return o, nil }
