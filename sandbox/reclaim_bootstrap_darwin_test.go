package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinDirectoryBootstrapFlags(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "dir")
	if err := os.Mkdir(dir, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "inner"), 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("outside"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	if err := os.Symlink(outside, filepath.Join(parent, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "file"), nil, 0400); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	linkBefore, err := os.Lstat(filepath.Join(parent, "link"))
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the kernel directly so the negative controls still prove safety
	// if a directory becomes a link/file after the helper's identity check.
	for _, name := range []string{"dir", "link", "link/inner", "file"} {
		err := unix.Fchmodat(int(f.Fd()), name+"/", 0700, reclaimNoFollowAny)
		t.Logf("%s: %v", name, err)
		if name == "dir" && err != nil {
			t.Fatal(err)
		}
		if name == "file" && !errors.Is(err, unix.ENOTDIR) {
			t.Fatalf("file: %v", err)
		}
		if (name == "link" || name == "link/inner") && !errors.Is(err, unix.ELOOP) {
			t.Fatalf("link: %v", err)
		}
	}
	for path, mode := range map[string]os.FileMode{
		dir:                              0700,
		filepath.Join(parent, "file"):    0400,
		outside:                          0555,
		filepath.Join(outside, "inner"):  0555,
		filepath.Join(outside, "marker"): 0400,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode changed: %v %v", path, info, err)
		}
	}
	after, err := os.Lstat(filepath.Join(parent, "link"))
	if err != nil || after.Mode() != linkBefore.Mode() {
		t.Fatal("symlink mode changed", err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "marker"))
	if err != nil || string(data) != "outside" {
		t.Fatal("outside contents changed", err)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("outside changed", err)
	}
}
