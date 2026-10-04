//go:build unix

package testenv

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// probeUnixSocket listens on and dials a socket in a fresh directory directly
// under os.TempDir, the base a restricted session's channel directory uses.
// The name is random, so a directory a killed probe left behind is harmless.
func probeUnixSocket() error {
	// Match shortPrivateDir's directory limit, not the kernel socket limit.
	channel := filepath.Join(os.TempDir(), "agent-harness-"+strconv.Itoa(os.Getuid()), "1234567890")
	if len(channel) > 90 {
		return &Refusal{Op: "socket path", Err: fmt.Errorf("%w: %d bytes", ErrSocketPathTooLong, len(channel))}
	}
	name := make([]byte, 4)
	_, _ = rand.Read(name)
	dir := filepath.Join(os.TempDir(), "ahp-"+hex.EncodeToString(name))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return &Refusal{Op: "mkdir", Err: err}
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		return &Refusal{Op: "listen", Err: err}
	}
	defer l.Close()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return &Refusal{Op: "connect", Err: err}
	}
	return conn.Close()
}

// probeProcessGroup starts a no-op shell in a process group of its own, as
// process.New does for every harness, and always reaps it.
func probeProcessGroup() error {
	cmd := exec.Command("/bin/sh", "-c", ":")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		return &Refusal{Op: "start with setpgid", Err: err}
	}
	return nil
}

// probeProcessStatus asks ps for this process's own status.
func probeProcessStatus() error {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return &Refusal{Op: "ps", Err: err}
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return errors.New("ps reported no status for this process")
	}
	return nil
}

// probeGroupPriority lowers the priority of a waiting shell's own process
// group, then lets it exit and reaps it.
func probeGroupPriority() error {
	cmd := exec.Command("/bin/sh", "-c", "read _")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return &Refusal{Op: "start with setpgid", Err: err}
	}
	err = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, 10)
	_ = stdin.Close()
	_ = cmd.Wait()
	if err != nil {
		return &Refusal{Op: "setpriority", Err: err}
	}
	return nil
}
