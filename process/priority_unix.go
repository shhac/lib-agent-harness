//go:build unix

package process

import "syscall"

// backgroundNice is the nice value a background tree runs at: well below
// interactive work, still above the idle band.
const backgroundNice = 10

// lowerPriority nices the contained process group; descendants inherit it.
// macOS's background band (PRIO_DARWIN_BG) is deliberately not used: it also
// throttles disk I/O and confines work to efficiency cores, and on a busy
// machine that made a 5-second test suite take two minutes and time out.
func lowerPriority(pid int) error {
	return syscall.Setpriority(syscall.PRIO_PGRP, pid, backgroundNice)
}
