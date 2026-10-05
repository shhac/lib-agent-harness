//go:build darwin || linux

package sandbox

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

func prepareCommandState(ctx context.Context, s *Sandbox, o *Options) error {
	return prepareCommandStateRemoving(ctx, s, o, removeCommandTree)
}

// The remover parameter lets tests exercise per-entry failure without
// weakening settlement or changing a global hook while other callers run.
func prepareCommandStateRemoving(ctx context.Context, s *Sandbox, o *Options, remove func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	base := filepath.Join(o.RuntimeHome, "commands")
	if err := os.MkdirAll(base, 0700); err != nil {
		return stateError(StateUnusable)
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || !privateStateDir(info) {
		return stateError(StateUnusable)
	}
	// Serialize sweeping and creation: a new directory must not be swept in
	// the interval between mkdir and taking its lifetime lock.
	var baseLock *os.File
	for {
		baseLock, err = lockSession(base)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrStateLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer baseLock.Close()
	entries, err := os.ReadDir(base)
	if err != nil {
		return stateError(StateUnusable)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		lock, err := lockSession(dir)
		if err != nil {
			continue
		} // locked or uncertain ownership is preserved
		if sweepCommandState(dir) == nil {
			_ = remove(dir)
		}
		_ = lock.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(base, "sandbox-")
	if err != nil {
		return stateError(StateUnusable)
	}
	lock, err := lockSession(dir)
	if err != nil {
		_ = remove(dir)
		return err
	}
	s.dir, s.lock = dir, lock

	return nil
}

func sweepCommandState(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "workbench-token.json"))
	if os.IsNotExist(err) {
		return nil
	} // interrupted token write, no launch
	if err != nil {
		return stateError(StateUnusable)
	}
	var token workbenchToken
	if json.Unmarshal(data, &token) != nil || len(token.Token) != 32 || token.Since.IsZero() || token.Since.After(time.Now().Add(time.Second)) {
		return stateError(StateUnusable)
	}
	if _, err := hex.DecodeString(token.Token); err != nil {
		return stateError(StateUnusable)
	}
	return process.SweepToken(token.Token, token.Since)
}
