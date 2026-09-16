package completion

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ensureDirectory accepts a canonical existing root and single path components.
// OpenRoot anchors creation to that root even if a child path is replaced.
func ensureDirectory(root string, components ...string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("state directory must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("state directory must be a real directory, not a symlink")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { handle.Close() }()
	path := root
	for _, name := range components {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:") {
			return "", errors.New("invalid private directory name")
		}
		info, err = handle.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if err = handle.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = handle.Lstat(name)
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("private state directory %q must not be a symlink or file", name)
		}
		child, err := handle.OpenRoot(name)
		if err != nil {
			return "", err
		}
		if runtime.GOOS == "windows" {
			// Windows chmod changes file attributes, not the ACL. This directory
			// inherits the caller-provided private root ACL; chmod only ensures
			// it is writable through the anchored directory handle.
			err = child.Chmod(".", 0700)
		} else {
			var f *os.File
			f, err = child.Open(".")
			if err == nil {
				err = f.Chmod(0700)
				f.Close()
			}
		}
		if err != nil {
			child.Close()
			return "", err
		}
		handle.Close()
		handle = child
		path = filepath.Join(path, name)
	}
	return path, nil
}
