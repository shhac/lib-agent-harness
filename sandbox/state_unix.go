//go:build !windows

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// privateStateDir reports whether only the owner can read or enter a
// directory.
func privateStateDir(info fs.FileInfo) bool { return info.Mode().Perm()&0o077 == 0 }

// lockSession takes the exclusive lock an open session holds on its
// directory. flock conflicts between separate opens even within one process,
// and the operating system releases it when the process dies.
func lockSession(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, stateError(StateUnusable)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, stateError(StateLocked)
		}
		return nil, stateError(StateUnusable)
	}
	return f, nil
}
