package wsfile

import (
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const MountCheckSupported = true

const OpenFlags = os.O_RDONLY | unix.O_NONBLOCK | unix.O_NOCTTY
const DirectoryFlags = OpenFlags | unix.O_DIRECTORY

func MountID(f *os.File) (Mount, error) {
	var st unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &st); err == nil && st.Mask&unix.STATX_MNT_ID != 0 {
		return Mount{ID: st.Mnt_id}, nil
	}
	data, err := os.ReadFile("/proc/self/fdinfo/" + strconv.FormatUint(uint64(f.Fd()), 10))
	if err == nil {
		return parseMountID(string(data))
	}
	return Mount{}, ErrMountUnavailable
}
func parseMountID(data string) (Mount, error) {
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && key == "mnt_id" {
			n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err == nil && n != 0 {
				return Mount{ID: n}, nil
			}
		}
	}
	return Mount{}, ErrMountUnavailable
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
