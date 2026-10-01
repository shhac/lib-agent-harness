package process

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// candidates are this user's processes started at or after since.
func candidates(since time.Time) []candidate {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil
	}
	var out []candidate
	for _, proc := range procs {
		started := time.Unix(proc.Proc.P_starttime.Unix())
		if !started.Before(since) && proc.Proc.P_stat != 5 { // SZOMB is not running.
			sec, nsec := proc.Proc.P_starttime.Unix()
			out = append(out, candidate{pid: int(proc.Proc.P_pid), parent: int(proc.Eproc.Ppid), group: int(proc.Eproc.Pgid), identity: fmt.Sprintf("%d:%d", sec, nsec)})
		}
	}
	return out
}

func processIdentity(pid int) string {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || p == nil || int(p.Proc.P_pid) != pid {
		return ""
	}
	sec, nsec := p.Proc.P_starttime.Unix()
	return fmt.Sprintf("%d:%d", sec, nsec)
}

// environment reads a process's environment from the kernel's copy of its
// arguments: argc, the executable path, padding, argc arguments, then the
// environment, each NUL terminated. macOS 26 leaves the environment out for
// its own platform binaries (/bin/sh, /bin/sleep), so those are found only as
// descendants of a marked process.
func environment(pid int) []string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	fields := bytes.Split(raw[4:], []byte{0})
	i := 1
	for i < len(fields) && len(fields[i]) == 0 {
		i++
	}
	var env []string
	for end := i + argc; i < end && i < len(fields); i++ {
		if token, ok := strings.CutPrefix(string(fields[i]), "--agent-harness-token="); ok && len(token) == 32 {
			env = append(env, launchVariable+"="+token)
		}
	}
	for ; i < len(fields) && len(fields[i]) > 0; i++ {
		env = append(env, string(fields[i]))
	}
	return env
}
