//go:build unix

package testenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnixSocketPathLimitBeforeBind(t *testing.T) {
	// No directory exists at this path: a refusal proves we never attempted mkdir or bind.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("x", 100)))
	err := probeUnixSocket()
	var refusal *Refusal
	if !errors.As(err, &refusal) || !errors.Is(err, ErrSocketPathTooLong) || refusal.Op != "socket path" {
		t.Fatalf("expected the library path limit, got %v", err)
	}
	if _, err := os.Stat(os.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("probe created a directory: %v", err)
	}
}
