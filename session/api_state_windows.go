package session

import (
	"io/fs"
	"os"

	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

func privateStateDir(info fs.FileInfo) bool { return sandboxbridge.PrivateStateDir(info) }
func lockSession(dir string) (*os.File, error) {
	f, err := sandboxbridge.LockState(dir)
	return f, fromSandbox(err, Options{})
}

// syncDir does nothing on Windows, which offers no directory sync; the
// entry's own content is already flushed.
func syncDir(string) {}
