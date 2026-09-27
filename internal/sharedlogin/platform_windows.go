package sharedlogin

import (
	"errors"
	"io/fs"
	"os"
)

// Windows has no owner-only mode check or no-follow open here, so a shared
// login is refused rather than copied without them.
func supported() bool { return false }

var errUnsupported = errors.New("shared login is not available on this platform")

func openNoFollow(string) (*os.File, error) { return nil, errUnsupported }
func ownerOnly(fs.FileInfo) bool            { return false }
func lock(string) (func(), error)           { return nil, errUnsupported }
