package sandbox

import (
	"crypto/rand"
	"encoding/hex"
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
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cryptographic random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
