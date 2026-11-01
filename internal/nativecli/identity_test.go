package nativecli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEdgeIdentityIncludesVendoredBinary(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "bin", "codex.js")
	vendor := filepath.Join(root, "vendor", "aarch64-apple-darwin", "codex", "codex")
	for _, dir := range []string{filepath.Dir(launcher), filepath.Dir(vendor)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{launcher: "launcher", filepath.Join(root, "package.json"): `{"name":"@openai/codex"}`, vendor: "native-one"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := EdgeIdentity(launcher)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(vendor, []byte("native-two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := EdgeIdentity(launcher)
	if err != nil || second == first {
		t.Fatal("vendor change not bound", err)
	}
}
