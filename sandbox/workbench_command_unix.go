//go:build darwin || linux

package sandbox

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/process"
)

type workbenchToken struct {
	Token string
	Since time.Time
}

// Counts actual canary runs, so tests can distinguish evidence reuse from a
// second proof without replacing any verification mechanism.
var workbenchCanaryRuns atomic.Uint64

// Notification proves setup completed; cmd.Process proves Start itself
// succeeded. A setup refusal can happen between those observations. Call only
// after Run settles, when exec.Cmd is no longer being mutated.
func settledCommandLaunch(cmd *exec.Cmd, notified bool, onStart func()) bool {
	if !notified && cmd.Process != nil {
		notifyCommandLaunch(onStart)
		return true
	}
	return notified
}

func commandSetupFailed(cmd *exec.Cmd, notified bool, runErr error) bool {
	return !notified && cmd.Process != nil && runErr != nil
}

func prepareWorkbenchCommands(o Options, proof Proof, dir string) (layout workbenchLayout, env []string, token workbenchToken, scratch string, err error) {
	tokenPath := filepath.Join(dir, "workbench-token.json")
	if data, err := os.ReadFile(tokenPath); err == nil {
		var old workbenchToken
		if json.Unmarshal(data, &old) != nil || len(old.Token) != 32 || old.Since.IsZero() || old.Since.After(time.Now().Add(time.Second)) {
			return layout, nil, token, scratch, stateError(StateUnusable)
		}
		if _, err := hex.DecodeString(old.Token); err != nil {
			return layout, nil, token, scratch, stateError(StateUnusable)
		}
		if process.SweepToken(old.Token, old.Since) != nil {
			return layout, nil, token, scratch, stateError(StateUnusable)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return layout, nil, token, scratch, stateError(StateUnusable)
	}
	scratch = filepath.Join(dir, "workbench")
	if err := os.RemoveAll(scratch); err != nil {
		return layout, nil, token, scratch, stateError(StateUnusable)
	}
	for _, name := range []string{"home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(scratch, name), 0700); err != nil {
			os.RemoveAll(scratch)
			return layout, nil, token, scratch, stateError(StateUnusable)
		}
	}
	system := proof.system
	if len(system) == 0 {
		os.RemoveAll(scratch)
		return layout, nil, token, scratch, stateError(StateUnusable)
	}
	token = workbenchToken{process.NewToken(), time.Now()}
	data, _ := json.Marshal(token)
	// Preserve the previous durable marker if a replacement is interrupted.
	tokenTemp := filepath.Join(dir, "workbench-token.tmp")
	_ = os.Remove(tokenTemp)
	f, err := os.OpenFile(tokenTemp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = os.Rename(tokenTemp, tokenPath)
	}
	if err != nil {
		_ = os.Remove(tokenTemp)
	}
	if err == nil {
		r, e := os.OpenRoot(dir)
		if e == nil {
			err = SyncDir(r)
			r.Close()
		} else {
			err = e
		}
	}
	if err != nil {
		os.RemoveAll(scratch)
		return layout, nil, token, scratch, stateError(StateUnusable)
	}
	layout = workbenchLayout{Work: o.WorkDir, Home: filepath.Join(scratch, "home"), Tmp: filepath.Join(scratch, "tmp"), System: system, Read: o.Read, Write: o.Write, Loopback: o.Loopback}
	env, err = skills.Environment(os.Environ(), append(append([]string{}, o.Env...), "HOME="+layout.Home, "TMPDIR="+layout.Tmp))
	if err != nil {
		os.RemoveAll(scratch)
		return layout, nil, token, scratch, stateError(StateUnusable)
	}
	env = process.TokenEnvironment(env, token.Token)
	if err = layout.anchorScratch(); err != nil {
		os.RemoveAll(scratch)
		return layout, nil, token, scratch, err
	}
	return layout, env, token, scratch, nil
}

type workbenchOutput struct {
	mu        sync.Mutex
	frozen    bool
	data      []byte
	limit     int
	truncated bool
}

func (b *workbenchOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.frozen {
		return n, nil
	}
	left := b.limit - len(b.data)
	if left < len(p) {
		p = p[:left]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *workbenchOutput) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.textLocked()
}
func (b *workbenchOutput) textLocked() string {
	s := string(b.data)
	if b.truncated {
		s += "\n[output truncated]"
	}
	return s
}
func (b *workbenchOutput) finish() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frozen = true
	return b.textLocked(), b.truncated
}

type bwrapStatus struct {
	ChildPID int  `json:"child-pid"`
	ExitCode *int `json:"exit-code"`
}

func readBwrapStatus(r io.Reader, onStart func()) (started bool, code int, settled bool) {
	code = -1
	d := json.NewDecoder(io.LimitReader(r, 16<<10))
	for {
		var s bwrapStatus
		if d.Decode(&s) != nil {
			return
		}
		if s.ChildPID > 0 {
			if !started && onStart != nil {
				onStart()
			}
			started = true
		}
		if s.ExitCode != nil && started && *s.ExitCode >= 0 && *s.ExitCode <= 255 {
			code = *s.ExitCode
			settled = true
		}
	}
}

func init() { sandboxhook.CanaryRuns = workbenchCanaryRuns.Load }
