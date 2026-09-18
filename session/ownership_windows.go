package session

import (
	"errors"
	"io/fs"
	"os"
)

// Windows has no equivalent here yet: this library contains a harness with a
// job object rather than a process group, and it has no advisory lock whose
// release proves a subtree is gone. A restricted session therefore reports its
// runtime as unavailable instead of running without containment.
func restrictedPlatform() bool { return false }

func ownerOnly(fs.FileInfo) error {
	return errors.New("restricted tool hosting is not available on this platform")
}

func holdBridgeLock(string) (*os.File, error) {
	return nil, errors.New("restricted tool hosting is not available on this platform")
}

func readBridgeLock(string) (*owner, error) {
	return nil, errors.New("restricted tool hosting is not available on this platform")
}

func terminateGroup(int) error {
	return errors.New("restricted tool hosting is not available on this platform")
}
