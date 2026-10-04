//go:build unix

package process

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
)

// backgroundNice is the nice value a background tree runs at: well below
// interactive work, still above the idle band.
const backgroundNice = 10

// Replaced in tests to stand for a setpriority that hasn't taken effect yet.
var (
	setPriority = syscall.Setpriority
	getPriority = syscall.Getpriority
)

// lowerPriority nices the contained process group; descendants inherit it.
// macOS's background band (PRIO_DARWIN_BG) is deliberately not used: it also
// throttles disk I/O and confines work to efficiency cores, and on a busy
// machine that made a 5-second test suite take two minutes and time out.
//
// A successful setpriority has been seen, rarely, to leave the leader at its
// parent's priority on GitHub's macOS runners, so the start is reported only
// once the leader reads back at backgroundNice.
func lowerPriority(pid int) error {
	for range 50 {
		if err := setPriority(syscall.PRIO_PGRP, pid, backgroundNice); err != nil {
			return err
		}
		if nice, err := readNice(pid); err == nil && nice == backgroundNice {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("process group %d did not take background priority %d", pid, backgroundNice)
}

// readNice reads a process's nice value. Linux's raw getpriority returns
// 20 - nice; macOS's returns the nice value itself.
func readNice(pid int) (int, error) {
	v, err := getPriority(syscall.PRIO_PROCESS, pid)
	if err != nil {
		return 0, err
	}
	if runtime.GOOS == "linux" {
		return 20 - v, nil
	}
	return v, nil
}
