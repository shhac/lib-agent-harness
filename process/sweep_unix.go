//go:build !windows

package process

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"sync/atomic"
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

type candidate struct {
	pid, parent, group int
	identity           string
}

// readEnvironment is an atomic test seam; production uses environment.
var readEnvironment = func() *atomic.Value {
	v := &atomic.Value{}
	v.Store(environment)
	return v
}()

// tree snapshots marked roots, a caller-validated group, and their descendants.
// Remembered roots must retain their birth identity.
func tree(token string, since time.Time, group int, remembered map[int]string) map[int]string {
	return treeFrom(candidates(since.Add(-time.Second)), token, group, remembered, readEnvironment.Load().(func(int) []string))
}

func treeFrom(procs []candidate, token string, group int, remembered map[int]string, readEnv func(int) []string) map[int]string {
	identities := map[int]string{}
	children := map[int][]int{}
	for _, proc := range procs {
		identities[proc.pid] = proc.identity
		children[proc.parent] = append(children[proc.parent], proc.pid)
	}
	doomed := map[int]string{}
	var mark func(int)
	mark = func(pid int) {
		if _, ok := doomed[pid]; ok || pid == os.Getpid() || pid <= 1 || identities[pid] == "" {
			return
		}
		doomed[pid] = identities[pid]
		for _, child := range children[pid] {
			mark(child)
		}
	}
	for _, proc := range procs {
		if carriesToken(readEnv(proc.pid), token) ||
			(group > 0 && proc.group == group) ||
			(remembered[proc.pid] != "" && remembered[proc.pid] == proc.identity) {
			mark(proc.pid)
		}
	}
	return doomed
}

func killTree(snapshot map[int]string) {
	killTreeWith(snapshot, processIdentity, func(pid int) {
		if group, err := syscall.Getpgid(pid); err == nil && group == pid {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
}

func killTreeWith(snapshot map[int]string, current func(int) string, signal func(int)) {
	for pid, identity := range snapshot {
		signalOwned(pid, identity, current, signal)
	}
}

// sweep retains live owned roots across rounds after their marked parent dies.
// Broken ancestry before enumeration and forks during killing remain best-effort.
func sweep(token string, since time.Time) {
	remembered := map[int]string{}
	for range 5 {
		snapshot := tree(token, since, 0, remembered)
		if len(snapshot) == 0 {
			return
		}
		for pid, identity := range snapshot {
			remembered[pid] = identity
		}
		killTree(snapshot)
		time.Sleep(50 * time.Millisecond)
	}
}

func signalOwned(pid int, identity string, current func(int) string, signal func(int)) {
	if identity != "" && current(pid) == identity {
		signal(pid)
	}
}
