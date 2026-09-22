package process

import (
	"context"
	"os/exec"
)

// Command builds a contained command in one step. Containment has two halves:
// New prepares the process group or job, and the command's Cancel must stop it.
// exec.CommandContext's own Cancel kills only the direct child, so a caller that
// forgets the second half leaves descendants running after cancellation. Here
// both are wired before the command is returned.
//
// The caller still owns the rest of the command (directory, environment,
// streams, WaitDelay), must start it with Process.Run and must Close the
// Process. Leave SysProcAttr alone: containment set it.
func Command(ctx context.Context, name string, args ...string) (*exec.Cmd, *Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	p, err := New(cmd)
	if err != nil {
		return nil, nil, err
	}
	cmd.Cancel = func() error { p.Stop(); return nil }
	return cmd, p, nil
}
