//go:build !windows

package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errUnsafeRuntimePath = errors.New("runtime path is a link or not a plain file or folder")

// writeBeneath replaces root/rel with a fresh owner-only file holding data,
// for state the library writes outside any sandbox into a runtime home a
// session could have changed. Nothing is followed: every folder from root
// down is opened without following a link and made where missing, and a
// folder that is a link or not a folder, or a target that is a link or not a
// plain file, is refused. The data goes into a new file beside the target,
// renamed over it, so a hard link planted at the target keeps its old inode
// rather than receiving the write.
func writeBeneath(root, rel string, data []byte) error {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == "." || strings.HasPrefix(rel, "..") {
		return errUnsafeRuntimePath
	}
	const folder = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dir, err := unix.Open(root, folder, 0)
	if err != nil {
		return errUnsafeRuntimePath
	}
	defer func() { _ = unix.Close(dir) }()
	if parent := filepath.Dir(rel); parent != "." {
		for _, part := range strings.Split(parent, string(filepath.Separator)) {
			next, err := unix.Openat(dir, part, folder, 0)
			if errors.Is(err, unix.ENOENT) {
				if err = unix.Mkdirat(dir, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
					return errUnsafeRuntimePath
				}
				next, err = unix.Openat(dir, part, folder, 0)
			}
			if err != nil {
				return errUnsafeRuntimePath
			}
			_ = unix.Close(dir)
			dir = next
		}
	}
	name := filepath.Base(rel)
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errUnsafeRuntimePath
	} else if err != nil && !errors.Is(err, unix.ENOENT) {
		return errUnsafeRuntimePath
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temporary := "." + name + "-" + hex.EncodeToString(suffix)
	fd, err := unix.Openat(dir, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errUnsafeRuntimePath
	}
	for written := 0; err == nil && written < len(data); {
		var n int
		n, err = unix.Write(fd, data[written:])
		written += n
	}
	if err == nil {
		err = unix.Fsync(fd)
	}
	if closeErr := unix.Close(fd); err == nil {
		err = closeErr
	}
	if err == nil {
		err = unix.Renameat(dir, temporary, dir, name)
	}
	if err != nil {
		_ = unix.Unlinkat(dir, temporary, 0)
		return errUnsafeRuntimePath
	}
	return nil
}
