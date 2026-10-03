package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// privateStateDir accepts any directory on Windows. Mode bits do not express
// a Windows access policy, so the caller supplies a RuntimeHome that already
// has a private ACL, which the files created in it inherit.
func privateStateDir(fs.FileInfo) bool { return true }

// lockSession takes the exclusive lock an open session holds on its
// directory. LockFileEx conflicts between separate handles even within one
// process, and the operating system releases it when the process dies.
func lockSession(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, stateError(StateUnusable)
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, stateError(StateLocked)
		}
		return nil, stateError(StateUnusable)
	}
	return f, nil
}
