package wsfile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestWindowsDeviceAndJunction(t *testing.T) {
	rootName := t.TempDir()
	root, err := os.Open(rootName)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mount, err := MountID(root)
	if err != nil {
		t.Fatal(err)
	}
	device, err := os.Open("NUL")
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	facts, err := Check(device, mount)
	if err != nil || facts.Regular || facts.Directory {
		t.Fatalf("%+v %v", facts, err)
	}
	sibling := t.TempDir()
	junction := filepath.Join(rootName, "junction")
	command := exec.Command("cmd", "/c", "mklink", "/J", junction, sibling)
	output, err := command.CombinedOutput()
	if err != nil {
		text := strings.ToLower(string(output))
		if testenv.Refused(err) || strings.Contains(text, "access is denied") || strings.Contains(text, "sufficient privilege") {
			testenv.SkipIfRefused(t, "creating a junction", fmt.Errorf("%w: %s", os.ErrPermission, output))
		} else {
			t.Fatalf("creating a junction: %v: %s", err, output)
		}
	}
	f, err := os.Open(junction)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	facts, err = Check(f, mount)
	if err != nil || facts.SameMount {
		t.Fatalf("%+v %v", facts, err)
	}
}

func TestWindowsMountIdentityLongPath(t *testing.T) {
	rootName := t.TempDir()
	root, err := os.Open(rootName)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mount, err := MountID(root)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(rootName, strings.Repeat("a", 128), strings.Repeat("b", 128), "file")
	if err = os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	facts, err := Check(f, mount)
	if err != nil || !facts.SameMount || !facts.Regular {
		t.Fatalf("%+v %v", facts, err)
	}
}
