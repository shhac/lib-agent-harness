package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// candidates are the processes whose environment this user can read. Linux
// reports start times in clock ticks since boot, so since is not used: the
// token alone decides.
// Every environment is readable to its owner, so parents are not needed.
func candidates(time.Time) []candidate {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []candidate
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err == nil {
			out = append(out, candidate{pid: pid})
		}
	}
	return out
}

func environment(pid int) []string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}
