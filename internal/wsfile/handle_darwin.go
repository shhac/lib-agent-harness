package wsfile

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const MountCheckSupported = true

const OpenFlags = os.O_RDONLY | unix.O_NONBLOCK | unix.O_NOCTTY
const DirectoryFlags = OpenFlags | unix.O_DIRECTORY

func MountID(f *os.File) (Mount, error) {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil {
		return Mount{}, ErrMountUnavailable
	}
	return Mount{ID: uint64(st.Dev)}, nil
}
func sameMount(a, b Mount) bool { return a.ID == b.ID }
func handleFacts(f *os.File) (uint64, bool, error) {
	var st unix.Stat_t
	err := unix.Fstat(int(f.Fd()), &st)
	return uint64(st.Nlink), true, err
}
func LinkCount(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}
func NotRegular(err error) bool {
	return err == unix.ENXIO || err == unix.ENODEV || err == unix.ENOTDIR
}
