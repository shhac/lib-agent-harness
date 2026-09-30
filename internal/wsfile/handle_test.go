//go:build linux || darwin || windows

package wsfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestOpenedHandleChecks(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenFile(dir, DirectoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mount, err := MountID(root)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := Check(root, mount)
	if err != nil || !facts.Directory || facts.Regular || !facts.SameMount {
		t.Fatalf("%+v %v", facts, err)
	}
	name := filepath.Join(dir, "file")
	if err = os.WriteFile(name, []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(name, OpenFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	facts, err = Check(f, mount)
	if err != nil || !facts.Regular || !facts.SameMount || facts.Links != 1 {
		t.Fatalf("%+v %v", facts, err)
	}
	testenv.SkipIfRefused(t, "creating a hard link", os.Link(name, filepath.Join(dir, "link")))
	facts, err = Check(f, mount)
	if err != nil || facts.Links != 2 {
		t.Fatalf("%+v %v", facts, err)
	}
	if runtime.GOOS != "windows" {
		device, err := os.OpenFile("/dev/null", OpenFlags, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer device.Close()
		facts, err = Check(device, mount)
		if err != nil || facts.Regular || facts.Directory {
			t.Fatalf("%+v %v", facts, err)
		}
	}
}
