package wsfile

import (
	"errors"
	"os"
	"testing"
)

func TestFDInfoMountIdentity(t *testing.T) {
	for _, data := range []string{"", "mnt_id: broken", "mnt_id: 0", "flags: 42"} {
		if _, err := parseMountID(data); !errors.Is(err, ErrMountUnavailable) {
			t.Fatalf("%q: %v", data, err)
		}
	}
	if m, err := parseMountID("pos: 0\nmnt_id:\t42\n"); err != nil || m.ID != 42 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestDistinctLinuxMounts(t *testing.T) {
	root, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	m, err := MountID(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/proc/self", "/dev/shm"} {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := Check(f, m)
		f.Close()
		if err != nil || facts.SameMount {
			t.Fatalf("%s: %+v %v", name, facts, err)
		}
	}
}
