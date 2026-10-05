//go:build darwin || linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
	"golang.org/x/sys/unix"
)

// removeCommandTree is called after settlement/a successful token sweep, or
// before any launch could have happened. Existing state stays under its
// lifetime lock through reclamation. Repair is best effort; boundary refusals
// and the anchored removal's error remain authoritative cleanup failures.
func removeCommandTree(path string) error {
	path = filepath.Clean(path)
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == ".." {
		return errors.New("command cleanup requires a child directory")
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Lstat(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("command cleanup requires a real directory")
	}
	f, err := parent.OpenFile(".", wsfile.DirectoryFlags, 0)
	if err != nil {
		return err
	}
	mount, err := wsfile.MountID(f)
	f.Close()
	if err != nil {
		return err
	}
	if err := restoreCommandDirectories(parent, base, info, mount); err != nil {
		return err
	}
	return parent.RemoveAll(base)
}

func restoreCommandDirectories(parent *os.Root, name string, info os.FileInfo, mount wsfile.Mount) (boundaryErr error) {
	// No-follow directory opens also refuse FIFOs. Linux O_PATH does not
	// require read permission; Darwin may need a directory-only bootstrap.
	parentFile, err := parent.OpenFile(".", wsfile.DirectoryFlags, 0)
	if err != nil {
		return
	}
	// Root.OpenFile resolves symlinks even with O_NOFOLLOW. Use openat on the
	// anchored parent handle to make the final no-follow requirement explicit.
	fd, err := unix.Openat(int(parentFile.Fd()), name, reclaimDirectoryFlags|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrPermission) {
		bootstrapErr := reclaimDirectoryBootstrap(parentFile, name, info, mount)
		if bootstrapErr == nil {
			fd, err = unix.Openat(int(parentFile.Fd()), name, reclaimDirectoryFlags|unix.O_CLOEXEC, 0)
		} else if errors.Is(bootstrapErr, errOtherMount) {
			parentFile.Close()
			return bootstrapErr
		}
	}
	parentFile.Close()
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return
	}
	facts, err := wsfile.Check(f, mount)
	if err != nil {
		return err
	}
	if !facts.Directory {
		return
	}
	if !facts.SameMount {
		return errOtherMount
	}
	if opened.Mode().Perm()&0700 != 0700 {
		if reclaimDirectoryChmod(f, opened.Mode()|0700) != nil {
			return
		}
	}
	r, err := parent.OpenRoot(name + "/.")
	if err != nil {
		return
	}
	defer r.Close()
	again, err := r.Stat(".")
	if err != nil || !os.SameFile(opened, again) {
		return
	}
	// The root now pins the checked directory; retain one handle per depth.
	f.Close()
	entries, err := r.Open(".")
	if err != nil {
		return
	}
	names, err := entries.Readdirnames(-1)
	entries.Close()
	if err != nil {
		return
	}
	for _, child := range names {
		childInfo, err := r.Lstat(child)
		if err == nil && childInfo.IsDir() && childInfo.Mode()&os.ModeSymlink == 0 {
			if err := restoreCommandDirectories(r, child, childInfo, mount); err != nil && boundaryErr == nil {
				boundaryErr = err
			}
		}
	}
	return
}
