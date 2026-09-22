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
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

var errOutputLimit = errors.New("model output limit exceeded")

type limitedOutput struct {
	exceeded bool
	buffer   bytes.Buffer
	max      int
	stop     func()
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > w.max-w.buffer.Len() {
		w.exceeded = true
		w.stop()
		return 0, errOutputLimit
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
	cmd, child, err := process.Command(ctx, bin, args...)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	cmd.WaitDelay = 2 * time.Second
	output := &limitedOutput{max: 2 * 1024 * 1024, stop: child.Stop}
	cmd.Stdout = output
	// Diagnostics may contain credentials or remote record content. Keep them out
	// of tool results, audit logs and model history.
	cmd.Stderr = io.Discard
	err = child.Run()
	if ctx.Err() != nil {
		return output.buffer.Bytes(), ctx.Err()
	}
	if output.exceeded {
		return output.buffer.Bytes(), errOutputLimit
	}
	return output.buffer.Bytes(), err
}
