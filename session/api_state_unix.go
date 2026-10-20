//go:build !windows

package session

import (
	"os"

	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

func lockSession(dir string) (*os.File, error) {
	f, err := sandboxbridge.LockState(dir)
	return f, fromSandbox(err, Options{})
}

// syncDir makes a new entry in dir durable. It is best effort: the entry's
// own content is already synced.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}
