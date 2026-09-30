//go:build !linux && !darwin && !windows

package wsfile

import (
	"io/fs"
	"os"
)

const MountCheckSupported = false

const OpenFlags = os.O_RDONLY
const DirectoryFlags = OpenFlags

func MountID(*os.File) (Mount, error)            { return Mount{}, ErrMountUnavailable }
func sameMount(a, b Mount) bool                  { return false }
func handleFacts(*os.File) (uint64, bool, error) { return 0, false, ErrMountUnavailable }
func LinkCount(fs.FileInfo) uint64               { return 0 }
func NotRegular(error) bool                      { return false }
