package session

// Owner-only files and directories for the tool channel and the restricted
// runtime, which hold its credential, its launch record and its locks.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// The restricted directory's layout: every file a restricted session keeps in
// the directory its caller supplies. Naming each once is what makes the lease
// Open reclaims under and the lease the tool host holds the same file.
func leasePath(dir string) string          { return filepath.Join(dir, "session.lease") }
func secretPath(dir string) string         { return filepath.Join(dir, "t.secret") }
func lockPath(dir string) string           { return filepath.Join(dir, "bridge.lock") }
func launchPath(dir string) string         { return filepath.Join(dir, "harness.launch") }
func pendingContextPath(dir string) string { return filepath.Join(dir, "context.pending") }
func catalogPath(dir string) string        { return filepath.Join(dir, "catalog.json") }

// shortPrivateDir creates an owner-only directory whose path leaves room for a
// local socket name. The system temporary directory is per-user on the
// platforms this runs on, and the directory itself is owner-only regardless.
func shortPrivateDir() (string, error) {
	base := filepath.Join(os.TempDir(), "agent-harness-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", errors.New("tool host could not prepare its private channel directory")
	}
	if err := os.Chmod(base, 0700); err != nil {
		return "", errors.New("tool host could not restrict its private channel directory")
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("tool host private channel directory is not usable")
	}
	if err = ownerOnly(info); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		return "", errors.New("tool host could not prepare its private channel directory")
	}
	if len(dir) > 90 {
		_ = os.RemoveAll(dir)
		return "", errors.New("tool host private channel path is too long for a local socket")
	}
	return dir, nil
}

// writePrivate replaces path with an owner-only file holding data. The data is
// written beside it and renamed into place, so a reader — or a later run after
// a crash — sees the old content or the new, never a truncated file, and a link
// planted at path is replaced rather than written through.
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return errors.New("could not write a private file")
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return errors.New("could not write a private file")
	}
	return nil
}
