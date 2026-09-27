//go:build !windows

package sharedlogin

import (
	"io/fs"
	"os"
	"syscall"
)

func supported() bool { return true }

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func ownerOnly(info fs.FileInfo) bool { return info.Mode().Perm()&0077 == 0 }

// lock serializes sharing and write-back across every process using this
// runtime home, so one worker's write-back never interleaves with another's
// share.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
