package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// candidates are the processes whose stat information this user can read. Linux
// reports start times in clock ticks since boot, so since is not used: the
// token alone decides.
// Parent links also cover descendants whose environment cannot be read.
func candidates(time.Time) []candidate {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []candidate
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err == nil {
			if identity := processIdentity(pid); identity != "" {
				fields := processFields(pid)
				if len(fields) >= 20 && fields[0] != "Z" {
					parent, _ := strconv.Atoi(fields[1])
					group, _ := strconv.Atoi(fields[2])
					out = append(out, candidate{pid: pid, parent: parent, group: group, identity: identity})
				}
			}
		}
	}
	return out
}

func processIdentity(pid int) string {
	fields := processFields(pid)
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func processFields(pid int) []string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil
	}
	// The command name may contain spaces or parentheses; the remaining
	// fields begin after its last closing parenthesis. Field 22 is starttime.
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(raw[i+1:]))
}

func environment(pid int) []string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}
