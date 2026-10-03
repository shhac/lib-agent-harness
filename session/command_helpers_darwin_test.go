package session

import (
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func requireWorkbenchSeatbelt(t *testing.T) {
	t.Helper()
	testenv.RequireProcessGroup(t)
	// A trivial profile isolates nesting refusal from generated-profile errors.
	trivial := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/bin/sh", "-c", "true")
	out, err := trivial.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		aborted := errors.As(err, &exit) && exit.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGABRT
		if aborted || strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
			err = errors.Join(fs.ErrPermission, err)
		}
		testenv.SkipIfRefused(t, "nested Seatbelt launch", err)
	}
}

func requireLegacyCommandPlatform(t *testing.T) { t.Helper(); requireWorkbenchSeatbelt(t) }
