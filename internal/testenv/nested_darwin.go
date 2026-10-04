package testenv

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func probeNestedSandbox() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/bin/sh", "-c", "true")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	aborted := errors.As(err, &exit) && exit.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGABRT
	if aborted || strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
		err = errors.Join(fs.ErrPermission, err)
	}
	return &Refusal{Op: "sandbox-exec", Err: err}
}
