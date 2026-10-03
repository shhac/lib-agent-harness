package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/wsfile"
)

// RemoveReserved removes only this session's temporaries beside a target file.
// A missing or refused parent is skipped, as in transcript recovery.
func (w *Workspace) RemoveReserved(ctx context.Context, rel, sessionID string) error {
	prefix := ".harness-workbench-" + strings.ReplaceAll(sessionID, "-", "") + "-"
	c, dir, _, code := w.writeParent(ctx, rel, false)
	if code != "" {
		return nil
	}
	f, err := dir.Open(".")
	if err != nil {
		c.close()
		return err
	}
	for {
		entries, e := f.ReadDir(workbenchDirBatch)
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, prefix) && wsfile.Reserved(name) && !entry.IsDir() {
				e := w.fault("remove_reserved")
				if e == nil {
					e = dir.Remove(path.Base(name))
				}
				if e != nil && !errors.Is(e, fs.ErrNotExist) {
					f.Close()
					c.close()
					return e
				}
			}
		}
		if e != nil {
			break
		}
	}
	f.Close()
	c.close()
	return nil
}
