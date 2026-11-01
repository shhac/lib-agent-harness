//go:build !windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteBeneathFollowsNoLink(t *testing.T) {
	owner := filepath.Join(t.TempDir(), "owner-config")
	if err := os.WriteFile(owner, []byte("owner"), 0600); err != nil {
		t.Fatal(err)
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		if data, err := os.ReadFile(owner); err != nil || string(data) != "owner" {
			t.Fatal("wrote through a link to a file outside the runtime home", string(data), err)
		}
	}
	t.Run("symlinked file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".gemini", "antigravity-cli"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(owner, filepath.Join(root, antigravitySettings)); err != nil {
			t.Fatal(err)
		}
		if err := writeBeneath(root, antigravitySettings, []byte("new")); !errors.Is(err, errUnsafeRuntimePath) {
			t.Fatal("symlinked target accepted", err)
		}
		unchanged(t)
	})
	t.Run("symlinked folder", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".gemini"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, ".gemini", "antigravity-cli")); err != nil {
			t.Fatal(err)
		}
		if err := writeBeneath(root, antigravitySettings, []byte("new")); !errors.Is(err, errUnsafeRuntimePath) {
			t.Fatal("symlinked intermediate folder accepted", err)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatal("wrote into the folder a link named")
		}
	})
	t.Run("hard-link alias", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, antigravitySettings)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		// A descendant-writable alias of the protected inode.
		alias := filepath.Join(root, "tmp-alias")
		if err := os.Link(target, alias); err != nil {
			t.Fatal(err)
		}
		if err := writeBeneath(root, antigravitySettings, []byte("new")); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(target); string(data) != "new" {
			t.Fatal("target not replaced", string(data))
		}
		if data, _ := os.ReadFile(alias); string(data) != "old" {
			t.Fatal("the write went through the hard link; the alias still names the loaded file", string(data))
		}
		targetInfo, _ := os.Stat(target)
		aliasInfo, _ := os.Stat(alias)
		if os.SameFile(targetInfo, aliasInfo) {
			t.Fatal("alias still shares the loaded inode")
		}
	})
	t.Run("not a plain file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, antigravitySettings), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeBeneath(root, antigravitySettings, []byte("new")); !errors.Is(err, errUnsafeRuntimePath) {
			t.Fatal("folder at the target accepted", err)
		}
	})
	t.Run("fresh owner-only file", func(t *testing.T) {
		root := t.TempDir()
		if err := writeBeneath(root, antigravitySettings, []byte("new")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(filepath.Join(root, antigravitySettings))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatal("not a fresh owner-only file", info, err)
		}
		for _, escape := range []string{"../x", "/etc/x"} {
			if err := writeBeneath(root, escape, nil); !errors.Is(err, errUnsafeRuntimePath) {
				t.Fatal("path outside the root accepted", escape)
			}
		}
	})
	t.Run("preparation refuses a planted link", func(t *testing.T) {
		o := antigravityEdgeOptions(t)
		if err := os.MkdirAll(filepath.Join(o.RuntimeHome, ".gemini", "config"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(owner, filepath.Join(o.RuntimeHome, antigravityMCPConfig)); err != nil {
			t.Fatal(err)
		}
		var failure *CapabilityError
		if err := antigravityEdge.prepare(t.Context(), o, nil); !errors.As(err, &failure) || failure.Code != CapabilityEdgeConfinementFailed {
			t.Fatal("prepared through a planted link", err)
		}
		unchanged(t)
	})
}
