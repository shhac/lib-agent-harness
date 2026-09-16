package completion

// The process containment and bounded output implementation is shared by both
// CLIs; only command construction and stream interpretation differ. It is
// stated once here rather than owned by either engine's adapter, so neither
// has to reach into the other to reuse it.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

type limitedOutput struct {
	buffer bytes.Buffer
	max    int
	stop   func()
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > w.max-w.buffer.Len() {
		w.stop()
		return 0, errors.New("Codex output limit exceeded")
	}
	return w.buffer.Write(p)
}

// runCLI invokes one contained CLI subprocess with bounded output, for either
// engine. cfg.run replaces execution entirely for synthetic tests; it is the
// single private seam, so a production path can never branch on which engine's
// hook happens to be set.
func runCLI(ctx context.Context, cfg Config, bin string, args []string, dir string, env []string, input string) ([]byte, error) {
	if cfg.run != nil {
		return cfg.run(ctx, bin, args, dir, env, input)
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	child, err := process.New(cmd)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	stop := child.Stop
	cmd.Cancel = func() error { stop(); return nil }
	cmd.WaitDelay = 2 * time.Second
	output := &limitedOutput{max: 2 * 1024 * 1024, stop: stop}
	cmd.Stdout = output
	// Diagnostics may contain credentials or remote record content. Keep them out
	// of tool results, audit logs and model history.
	cmd.Stderr = io.Discard
	err = child.Run()
	return output.buffer.Bytes(), err
}
