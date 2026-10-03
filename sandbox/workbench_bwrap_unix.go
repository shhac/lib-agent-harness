//go:build darwin || linux

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

const workbenchBwrapVersion = "bwrap-workbench-v2"

func workbenchBinaryFingerprint(binary string) (string, error) {
	info, e := os.Stat(binary)
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("sandbox binary is not regular")
	}
	data, e := os.ReadFile(binary)
	if e != nil {
		return "", e
	}
	content := sha256.Sum256(data)
	payload, _ := json.Marshal([]any{binary, info.Size(), info.ModTime(), info.Mode(), content})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

func bwrapSystemDirs() []string {
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/hosts", "/etc/localtime", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/ssl/certs", "/etc/ca-certificates", "/etc/alternatives", "/etc/os-release"}
}

func bwrapSystemContains(system, dir string) bool {
	if lexicallyWithin(system, dir) {
		return true
	}
	if _, e := os.Stat(system); os.IsNotExist(e) {
		return false
	}
	return nested(system, dir)
}

func linuxSystemPlacement(p string) bool {
	for _, system := range append(bwrapSystemDirs(), "/etc") {
		if bwrapSystemContains(system, p) {
			return true
		}
	}
	return false
}

func shallowPaths(paths []string) {
	sort.Slice(paths, func(i, j int) bool {
		a, b := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if a != b {
			return a < b
		}
		return paths[i] < paths[j]
	})
}

// bwrapArgs is the sole mount builder for both commands and their proof.
// Only phase 1 can cover mounts; all directories precede every host bind.
func bwrapArgs(l workbenchLayout) ([]string, error) {
	for _, p := range []string{l.Work, l.Home, l.Tmp} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || linuxSystemPlacement(p) {
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
			if bwrapSystemContains(system, p) {
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
	shallowPaths(paths)
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

func bwrapRefusal(code string) *ProofError {
	e := workbenchCapability(code)
	e.Tools = []string{"bwrap"}
	return e
}

func supportedBwrapVersion(output string) (string, bool) {
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[0] != "bubblewrap" {
		return "", false
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return "", false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return "", false
			}
		}
		n, e := strconv.ParseUint(p, 10, 32)
		if e != nil {
			return "", false
		}
		nums[i] = n
	}
	return fields[1], nums[0] > 0 || nums[1] >= 8
}

func checkBwrap(ctx context.Context, l workbenchLayout) (string, string, error) {
	binary, err := exec.LookPath("bwrap")
	if err != nil {
		return "", "", bwrapRefusal(CapabilitySandboxToolMissing)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", "", bwrapRefusal(CapabilitySandboxToolMissing)
	}
	identity, err := workbenchBinaryFingerprint(binary)
	if err != nil {
		return "", "", workbenchCapability(CapabilitySandboxUnavailable)
	}
	cmd, p, err := process.Command(ctx, binary, "--version")
	if err != nil {
		return "", "", bwrapRefusal(CapabilitySandboxToolOutdated)
	}
	out := &workbenchOutput{limit: 1024}
	cmd.Env = workbenchProbeEnvironment(l)
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	err = p.Run()
	p.Close()
	version, ok := supportedBwrapVersion(out.text())
	if err != nil || !ok {
		return "", "", bwrapRefusal(CapabilitySandboxToolOutdated)
	}
	args, err := bwrapArgs(l)
	if err != nil {
		return "", "", workbenchCapability(CapabilitySandboxUnavailable)
	}
	cmd, p, err = process.Command(ctx, binary, append(args, "--", "/bin/sh", "-c", "exit 0")...)
	if err != nil {
		return "", "", bwrapRefusal(CapabilitySandboxNamespacesUnavailable)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.Env = workbenchProbeEnvironment(l)
	cmd.WaitDelay = time.Second
	err = p.Run()
	p.Close()
	if err != nil {
		return "", "", bwrapRefusal(CapabilitySandboxNamespacesUnavailable)
	}
	current, err := workbenchBinaryFingerprint(binary)
	if err != nil || current != identity {
		return "", "", workbenchCapability(CapabilitySandboxUnavailable)
	}
	return binary, version, nil
}
