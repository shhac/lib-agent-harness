//go:build darwin || linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/internal/wsfile"
	"github.com/shhac/lib-agent-harness/process"
)

func readonlyCommandTree(t *testing.T, tree, outside string) {
	t.Helper()
	for _, dir := range []string{"a/b/c", "z"} {
		if err := os.MkdirAll(filepath.Join(tree, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "a/b/c/file"), []byte("inside"), 0400); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"out": outside, "in": "a/b"} {
		if err := os.Symlink(target, filepath.Join(tree, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"a/b/c", "a/b", "a"} {
		if err := os.Chmod(filepath.Join(tree, dir), 0555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "z/file"), []byte("unreadable"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(tree, "z"), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// These synthetic fixtures have no concurrent writers. Restore their
		// explicitly known directories even when the production helper fails,
		// so a failing regression cannot strand the check runner's own scratch.
		for _, rel := range []string{"", "a", "a/b", "a/b/c", "z"} {
			path := filepath.Join(tree, rel)
			if info, err := os.Lstat(path); err == nil && info.IsDir() {
				_ = os.Chmod(path, info.Mode()|0700)
			}
		}
		_ = removeCommandTree(tree)
	})
}

func readonlyOutside(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		testenv.SkipIfRefused(t, "non-root permission enforcement", syscall.EPERM)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	return outside
}

func assertOutsideUnchanged(t *testing.T, outside string) {
	t.Helper()
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("outside mode changed: %v %v", info, err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "file"))
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside contents changed: %q %v", data, err)
	}
	info, err = os.Stat(filepath.Join(outside, "file"))
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("outside file mode changed: %v %v", info, err)
	}
}

func TestRemoveCommandTreeReadonly(t *testing.T) {
	outside := readonlyOutside(t)
	plain := filepath.Join(t.TempDir(), "plain")
	readonlyCommandTree(t, plain, outside)
	if err := os.Chmod(plain, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(plain); err == nil {
		t.Fatal("fixture did not reproduce permission failure")
	}
	tree := filepath.Join(t.TempDir(), "tree")
	readonlyCommandTree(t, tree, outside)
	file, err := os.Open(filepath.Join(tree, "a/b/c/file"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Chmod(tree, 0000); err != nil {
		t.Fatal(err)
	}
	if err := removeCommandTree(tree); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := file.Stat()
	if err != nil || fileInfo.Mode().Perm() != 0400 {
		t.Fatalf("non-directory mode changed: %v %v", fileInfo, err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatal("tree survived", err)
	}
	assertOutsideUnchanged(t, outside)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := removeCommandTree(link); err == nil {
		t.Fatal("top-level symlink accepted")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("refused link removed", err)
	}
	assertOutsideUnchanged(t, outside)
	topFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(topFile, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := removeCommandTree(topFile); err == nil {
		t.Fatal("top-level file accepted")
	}
	info, err := os.Stat(topFile)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("refused file changed", err)
	}
	if err := removeCommandTree(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestCommandStateReadonlyRecovery(t *testing.T) {
	t.Run("before-token", func(t *testing.T) { testCommandStateReadonlyRecovery(t, false) })
	t.Run("after-token", func(t *testing.T) { testCommandStateReadonlyRecovery(t, true) })
}

func testCommandStateReadonlyRecovery(t *testing.T, withToken bool) {
	t.Helper()
	if withToken {
		testenv.RequireProcessStatus(t)
	}
	outside := readonlyOutside(t)
	o := commandStateOptions(t)
	base := filepath.Join(o.RuntimeHome, "commands")
	for _, name := range []string{"old", "no-token", "uncertain", "locked", "unremovable"} {
		tree := filepath.Join(base, name, "workbench", "tmp", "checkout")
		readonlyCommandTree(t, tree, outside)
	}
	if withToken {
		data, err := json.Marshal(workbenchToken{process.NewToken(), time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "old", "workbench-token.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "uncertain", "workbench-token.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSession(filepath.Join(base, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// Inject a removal fault for one settled entry; its siblings must still
	// be reclaimed and the new entry created.
	s := &Sandbox{}
	if err := prepareCommandStateRemoving(context.Background(), s, &o, func(path string) error {
		if filepath.Base(path) == "unremovable" {
			return errors.New("injected removal failure")
		}
		return removeCommandTree(path)
	}); err != nil {
		t.Fatal(err)
	}
	defer s.lock.Close()
	for _, name := range []string{"old", "no-token"} {
		if _, err := os.Stat(filepath.Join(base, name)); !os.IsNotExist(err) {
			t.Fatal(name, "survived", err)
		}
	}
	for _, name := range []string{"uncertain", "locked", "unremovable"} {
		for dir, mode := range map[string]os.FileMode{"a": 0555, "z": 0000} {
			info, err := os.Stat(filepath.Join(base, name, "workbench/tmp/checkout", dir))
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("%s touched before ownership decision: %v %v", name, info, err)
			}
		}
	}
	assertOutsideUnchanged(t, outside)
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatal("new entry not created", err)
	}
	next := &Sandbox{}
	if err := prepareCommandState(context.Background(), next, &o); err != nil {
		t.Fatal(err)
	}
	defer next.lock.Close()
	if _, err := os.Stat(filepath.Join(base, "unremovable")); !os.IsNotExist(err) {
		t.Fatal("failed entry not retried", err)
	}
}

func TestCommandRepairRefusesChangedDirectoryAndMount(t *testing.T) {
	outside := readonlyOutside(t)
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(tree, 0700)
		_ = os.Chmod(filepath.Join(dir, "original"), 0700)
	})
	info, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	f, err := parent.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	mount, err := wsfile.MountID(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	wrongMount := mount
	wrongMount.ID++
	if !errors.Is(restoreCommandDirectories(parent, "tree", info, wrongMount), errOtherMount) {
		t.Fatal("other mount not refused")
	}
	unchanged, err := os.Stat(tree)
	if err != nil || unchanged.Mode().Perm() != 0000 {
		t.Fatal("other mount mode changed", err)
	}
	if err := os.Rename(tree, filepath.Join(dir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tree); err != nil {
		t.Fatal(err)
	}
	restoreCommandDirectories(parent, "tree", info, mount)
	assertOutsideUnchanged(t, outside)
	// A different real directory must also fail the original Lstat identity.
	if err := os.Remove(tree); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tree, 0000); err != nil {
		t.Fatal(err)
	}
	restoreCommandDirectories(parent, "tree", info, mount)
	unchanged, err = os.Stat(tree)
	if err != nil || unchanged.Mode().Perm() != 0000 {
		t.Fatal("replacement directory touched", err)
	}
}
