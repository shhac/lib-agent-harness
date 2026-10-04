//go:build darwin || linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Frozen v0.21 template reconstruction. Filesystem observations are dynamic,
// but the profile-independent flags, ordering and path comparisons must never
// follow live builders during the stage-B move.
func legacyBwrapSystemDirs() []string {
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/hosts", "/etc/localtime", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/ssl/certs", "/etc/ca-certificates", "/etc/alternatives", "/etc/os-release"}
}

func legacyBwrapSystemContains(system, dir string) bool {
	if legacyWithin(system, dir) {
		return true
	}
	if _, e := os.Stat(system); os.IsNotExist(e) {
		return false
	}
	return legacyNested(system, dir)
}

func legacyLinuxSystemPlacement(p string) bool {
	for _, system := range append(legacyBwrapSystemDirs(), "/etc") {
		if legacyBwrapSystemContains(system, p) {
			return true
		}
	}
	return false
}

func legacyShallowPaths(paths []string) {
	sort.Slice(paths, func(i, j int) bool {
		a, b := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if a != b {
			return a < b
		}
		return paths[i] < paths[j]
	})
}

// legacyBwrapArgs is the sole mount builder for both commands and their proof.
// Only phase 1 can cover mounts; all directories precede every host bind.
func legacyBwrapArgs(l workbenchLayout) ([]string, error) {
	for _, p := range []string{l.Work, l.Home, l.Tmp} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || legacyLinuxSystemPlacement(p) {
			return nil, fmt.Errorf("invalid command sandbox placement")
		}
	}
	args := []string{"--unshare-user", "--disable-userns", "--cap-drop", "ALL", "--unshare-net", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup", "--die-with-parent", "--new-session", "--tmpfs", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run"}
	type mount struct {
		flag, source, dest string
		directory          bool
	}
	var systems, binds []mount
	for _, p := range l.System {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m := mount{"--ro-bind", p, p, info.IsDir()}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(p)
			if e != nil {
				return nil, e
			}
			m.flag, m.source = "--symlink", target
		}
		systems = append(systems, m)
	}
	for _, p := range l.Read {
		covered := false
		for _, system := range l.System {
			if legacyBwrapSystemContains(system, p) {
				covered = true
				break
			}
		}
		if !covered {
			binds = append(binds, mount{"--ro-bind", p, p, true})
		}
	}
	workFlag := "--ro-bind"
	if l.Write {
		workFlag = "--bind"
	}
	binds = append(binds, mount{workFlag, l.Work, l.Work, true}, mount{"--bind", l.Home, l.Home, true}, mount{"--bind", l.Tmp, l.Tmp, true})
	sort.Slice(binds, func(i, j int) bool {
		a, b := strings.Count(binds[i].dest, "/"), strings.Count(binds[j].dest, "/")
		if a != b {
			return a < b
		}
		if binds[i].dest == binds[j].dest {
			return binds[i].flag == "--ro-bind" && binds[j].flag == "--bind"
		}
		return binds[i].dest < binds[j].dest
	})
	dirs := map[string]bool{} // true denotes a public system destination
	if info, e := os.Lstat(filepath.Join(l.Work, ".git")); e == nil && info.IsDir() {
		dirs[filepath.Join(l.Work, ".git")] = false
	}
	for index, group := range [][]mount{systems, binds} {
		for _, m := range group {
			p := m.dest
			if !m.directory {
				p = filepath.Dir(p)
			}
			for p != "/" {
				dirs[p] = dirs[p] || index == 0
				p = filepath.Dir(p)
			}
		}
	}
	paths := make([]string, 0, len(dirs))
	for p := range dirs {
		paths = append(paths, p)
	}
	legacyShallowPaths(paths)
	for _, p := range paths {
		if p == "/dev" || p == "/proc" || p == "/tmp" || p == "/run" {
			continue
		}
		perms := "0700"
		if dirs[p] {
			perms = "0755"
		}
		args = append(args, "--perms", perms, "--dir", p)
	}
	for _, group := range [][]mount{systems, binds} {
		for _, m := range group {
			args = append(args, m.flag, m.source, m.dest)
		}
	}
	git := filepath.Join(l.Work, ".git")
	if info, err := os.Lstat(git); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("invalid metadata overlay")
		}
		args = append(args, "--ro-bind", git, git)
	} else if !os.IsNotExist(err) {
		return nil, err
	} else if l.Write {
		if _, e := os.Stat(l.Work); e == nil {
			return nil, fmt.Errorf("writable command workspace requires an existing .git overlay")
		}
	}
	return args, nil
}

func legacyWithin(outer, inner string) bool {
	outer, inner = filepath.Clean(outer), filepath.Clean(inner)
	return outer == inner || strings.HasPrefix(inner, strings.TrimRight(outer, "/")+"/")
}
func legacyNested(outer, inner string) bool {
	if legacyWithin(outer, inner) {
		return true
	}
	target, err := os.Stat(outer)
	if err != nil {
		return true
	}
	for dir := inner; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, target) {
			return true
		}
		if dir == filepath.Dir(dir) {
			return false
		}
	}
}
func TestWorkbenchBwrapTemplatePinned(t *testing.T) {
	requireBwrapSystemMetadata(t)
	root := t.TempDir()
	for _, name := range []string{"work/.git", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	args, err := bwrapArgs(workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), Write: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range args {
		args[i] = strings.ReplaceAll(arg, root, "/workbench-template")
	}
	want := []string{
		"--unshare-user", "--disable-userns", "--cap-drop", "ALL", "--unshare-net", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup", "--die-with-parent", "--new-session", "--tmpfs", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run",
		"--perms", "0700", "--dir", "/workbench-template",
		"--perms", "0700", "--dir", "/workbench-template/home",
		"--perms", "0700", "--dir", "/workbench-template/tmp",
		"--perms", "0700", "--dir", "/workbench-template/work",
		"--perms", "0700", "--dir", "/workbench-template/work/.git",
		"--bind", "/workbench-template/home", "/workbench-template/home",
		"--bind", "/workbench-template/tmp", "/workbench-template/tmp",
		"--bind", "/workbench-template/work", "/workbench-template/work",
		"--ro-bind", "/workbench-template/work/.git", "/workbench-template/work/.git",
	}
	// Ancestor destinations are host observations, not template text. Rebuild
	// only these varying paths; their flags, modes and root omission stay fixed.
	var ancestors []string
	for parent := filepath.Dir(root); parent != "/"; parent = filepath.Dir(parent) {
		if parent != "/tmp" && parent != "/run" && parent != "/dev" && parent != "/proc" {
			ancestors = append(ancestors, parent)
		}
	}
	var prefix []string
	for i := len(ancestors) - 1; i >= 0; i-- {
		prefix = append(prefix, "--perms", "0700", "--dir", ancestors[i])
	}
	want = append(append(append([]string{}, want[:21]...), prefix...), want[21:]...)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("bwrap template changed:\n%q\nwant:\n%q", args, want)
	}
	if !reflect.DeepEqual(bwrapSystemDirs(), legacyBwrapSystemDirs()) {
		t.Fatal("bwrap proof system list changed")
	}
}
