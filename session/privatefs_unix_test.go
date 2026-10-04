//go:build unix

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateChannelRejectsLongTMPDIR(t *testing.T) {
	// Keep in step with internal/testenv's socket probe; no bind is required.
	tmp := filepath.Join(t.TempDir(), strings.Repeat("x", 100))
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	h, err := newToolHost(ToolHost{
		Server: "workspace", Dir: dir, Handler: nopHandler(),
		Tools:  []ToolDefinition{{Name: "read_file", Description: "read", Schema: map[string]any{"type": "object"}}},
		Bridge: Bridge{Path: "/usr/bin/true", Args: []string{"tool-bridge"}},
	}, nil)
	if h != nil {
		h.close()
		t.Fatal("long TMPDIR opened a tool host")
	}
	if err == nil || !strings.Contains(err.Error(), "path is too long") {
		t.Fatalf("%v", err)
	}
}
