package session

// Owner-only files and directories for the tool channel and the restricted
// runtime, which hold its credential, its launch record and its locks.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

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

func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("tool host could not write its private channel credential")
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return errors.New("tool host could not write its private channel credential")
	}
	return f.Close()
}
