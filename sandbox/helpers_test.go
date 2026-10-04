package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func symlink(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if errors.Is(err, syscall.Errno(1314)) {
		// Windows without the privilege to create links.
		err = fmt.Errorf("%w: %v", fs.ErrPermission, err)
	}
	testenv.SkipIfRefused(t, "creating a symbolic link", err)
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		testenv.SkipIfRefused(t, "creating a workspace fixture", err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		testenv.SkipIfRefused(t, "creating a workspace fixture", err)
	}
}
