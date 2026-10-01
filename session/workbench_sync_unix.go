//go:build !windows

package session

import (
	"github.com/shhac/lib-agent-harness/internal/wsfile"
	"io/fs"
	"os"
)

func workbenchFileMode(mode fs.FileMode) fs.FileMode { return mode.Perm() }

func syncWorkbenchHandle(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Sync()
}

func syncWorkbenchDir(r *os.Root) error {
	f, err := r.OpenFile(".", wsfile.DirectoryFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
