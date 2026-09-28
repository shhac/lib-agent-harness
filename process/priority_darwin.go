package process

import "syscall"

// macOS's background band (setpriority PRIO_DARWIN_PROCESS, PRIO_DARWIN_BG):
// lowest CPU scheduling and throttled disk and network I/O, inherited by
// children. Checked on macOS 27 with a descendant forked into its own group.
const (
	prioDarwinProcess = 4
	prioDarwinBG      = 0x1000
)

func lowerPriority(pid int) error {
	if err := syscall.Setpriority(syscall.PRIO_PGRP, pid, backgroundNice); err != nil {
		return err
	}
	return syscall.Setpriority(prioDarwinProcess, pid, prioDarwinBG)
}
