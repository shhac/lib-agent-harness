//go:build unix && !darwin

package process

import "syscall"

func lowerPriority(pid int) error {
	return syscall.Setpriority(syscall.PRIO_PGRP, pid, backgroundNice)
}
