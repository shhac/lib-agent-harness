//go:build !windows

package process

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

// launchVariable carries one token per contained launch, space separated, so a
// launch nested inside another keeps its parent's token as well as its own.
// Every process the harness starts inherits it, including the ones its agent
// backgrounds into a process group of their own, which the group kill misses.
const launchVariable = "AGENT_HARNESS_LAUNCH"

func newLaunchToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// markedEnvironment returns env (the process's own when nil) with token added
// to the launch variable.
func markedEnvironment(env []string, token string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	tokens := []string{}
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, launchVariable+"="); ok {
			tokens = strings.Fields(value)
			continue
		}
		out = append(out, entry)
	}
	return append(out, launchVariable+"="+strings.Join(append(tokens, token), " "))
}

// carriesToken reports whether an environment block names the token.
func carriesToken(env []string, token string) bool {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, launchVariable+"="); ok && slices.Contains(strings.Fields(value), token) {
			return true
		}
	}
	return false
}

type candidate struct{ pid, parent int }

// sweep kills this user's processes that carry token, or descend from one that
// does, and started no earlier than since. It repeats while it still finds
// some, because a process may fork while its siblings are being killed. A
// process that cleared its environment, or a platform binary whose marked
// parent is already gone, escapes: this is a best-effort sweep, not a
// boundary.
func sweep(token string, since time.Time) {
	self := os.Getpid()
	for range 5 {
		procs := candidates(since.Add(-time.Second))
		children := map[int][]int{}
		for _, proc := range procs {
			children[proc.parent] = append(children[proc.parent], proc.pid)
		}
		doomed := map[int]bool{}
		var mark func(int)
		mark = func(pid int) {
			if doomed[pid] || pid == self || pid <= 1 {
				return
			}
			doomed[pid] = true
			for _, child := range children[pid] {
				mark(child)
			}
		}
		for _, proc := range procs {
			if carriesToken(environment(proc.pid), token) {
				mark(proc.pid)
			}
		}
		if len(doomed) == 0 {
			return
		}
		for pid := range doomed {
			if group, err := syscall.Getpgid(pid); err == nil && group == pid {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
