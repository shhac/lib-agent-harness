package wsfile

import (
	"os"
	"testing"
)

func TestDistinctDarwinMounts(t *testing.T) {
	root, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	m, err := MountID(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("/dev")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	facts, err := Check(f, m)
	if err != nil || facts.SameMount {
		t.Fatalf("%+v %v", facts, err)
	}
}
