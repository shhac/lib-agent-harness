package sandbox

import (
	"os"
	"syscall"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
	"golang.org/x/sys/unix"
)

const reclaimDirectoryFlags = unix.O_EVTONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK

func reclaimDirectoryChmod(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

// AT_SYMLINK_NOFOLLOW_ANY from the Darwin SDK's sys/fcntl.h. Our x/sys
// version does not name it. Unsupported kernels fail rather than falling
// back to AT_SYMLINK_NOFOLLOW, which follows links with a trailing slash.
const reclaimNoFollowAny = 0x0800

func reclaimDirectoryBootstrap(parent *os.File, name string, info os.FileInfo, mount wsfile.Mount) error {
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	expected, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Dev != expected.Dev || st.Ino != expected.Ino {
		return errChanged
	}
	if uint64(st.Dev) != mount.ID {
		return errOtherMount
	}
	// Darwin cannot open a 0000 directory for fchmod. The slash atomically
	// requires a directory, while NOFOLLOW_ANY refuses a swapped-in symlink
	// before changing anything. Unlike a plain no-follow chmod, this cannot
	// change a regular file or symlink's mode. The parent descriptor anchors
	// the single-component lookup; the caller checks the opened identity too.
	return unix.Fchmodat(int(parent.Fd()), name+"/", uint32(st.Mode&07777)|0700, reclaimNoFollowAny)
}
