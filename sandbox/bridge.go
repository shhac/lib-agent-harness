package sandbox

import (
	"context"

	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

func init() {
	sandboxbridge.NormalizeWorkbench = func(v any) (any, error) {
		return normalize(v.(Options), false)
	}
	// Session has already normalized options in its original refusal order.
	sandboxbridge.ProveWorkbench = func(ctx context.Context, v any) (any, error) {
		return proveWorkbench(ctx, v.(Options))
	}
	sandboxbridge.PrivateStateDir = privateStateDir
	sandboxbridge.LockState = lockSession
}
