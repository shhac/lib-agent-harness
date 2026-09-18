package session

import (
	"errors"
	"io/fs"
	"os"
)

// Windows has no equivalent here yet: this library contains a harness with a
// job object rather than a signalable process group, and it has no advisory
// lock whose release proves a subtree is gone. A restricted session therefore
// reports its runtime as unavailable instead of running without containment.
// Ordinary, unrestricted sessions are unaffected and behave as they always have.
func restrictedPlatform() bool { return false }

var errUnsupportedRuntime = errors.New("restricted tool hosting is not available on this platform")

func ownerOnly(fs.FileInfo) error { return errUnsupportedRuntime }

// openNoFollow has no restricted runtime to serve on this platform. It exists so
// the credential-sharing code builds everywhere; ordinary sessions never reach
// it, and a restricted session is refused before it could.
func openNoFollow(string) (*os.File, error)           { return nil, errUnsupportedRuntime }
func groupAlive(int) (bool, error)                    { return false, errUnsupportedRuntime }
func holdBridgeLock(string, string) (*os.File, error) { return nil, errUnsupportedRuntime }
func readBridgeLock(string) (*owner, error)           { return nil, errUnsupportedRuntime }
func holdLease(string) (*os.File, error)              { return nil, errUnsupportedRuntime }
func terminateGroup(int, int) error                   { return errUnsupportedRuntime }
