package sandbox

import (
	"errors"
	"os"
	"strconv"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
	"golang.org/x/sys/unix"
)

const reclaimDirectoryFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW

func reclaimDirectoryBootstrap(*os.File, string, os.FileInfo, wsfile.Mount) error {
	// O_PATH already handles unreadable directories. A denied open is an
	// ownership/containment failure, not a reason to weaken handle checks.
	return os.ErrPermission
}

func reclaimDirectoryChmod(f *os.File, mode os.FileMode) error {
	// O_PATH does not support fchmod. x/sys v0.28.0 dispatches this nonzero
	// AT_EMPTY_PATH request to fchmodat2, operating on the pinned handle.
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= unix.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		bits |= unix.S_ISGID
	}
	if mode&os.ModeSticky != 0 {
		bits |= unix.S_ISVTX
	}
	err := unix.Fchmodat(int(f.Fd()), "", bits, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		// Kernels without this operation need accessible procfs for the same
		// pinned handle. This is not a path into the untrusted tree; f remains
		// open throughout. A missing/inaccessible procfs returns an error.
		return os.Chmod("/proc/self/fd/"+strconv.FormatUint(uint64(f.Fd()), 10), mode)
	}
	return err
}
