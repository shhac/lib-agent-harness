//go:build darwin || linux

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func commandStateOptions(t *testing.T) Options {
	return Options{RuntimeHome: t.TempDir(), Workbench: &Workbench{Commands: &Commands{}}}
}

func TestCommandStateInterruptedAndUncertainEntries(t *testing.T) {
	o := commandStateOptions(t)
	base := filepath.Join(o.RuntimeHome, "commands")
	for _, name := range []string{"interrupted", "uncertain", "live"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "interrupted", "workbench-token.tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "uncertain", "workbench-token.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	liveLock, err := lockSession(filepath.Join(base, "live"))
	if err != nil {
		t.Fatal(err)
	}
	defer liveLock.Close()
	s := &CommandSandbox{}
	if err := prepareCommandState(context.Background(), s, &o); err != nil {
		t.Fatal(err)
	}
	defer s.lock.Close()
	if _, err := os.Stat(filepath.Join(base, "interrupted")); !os.IsNotExist(err) {
		t.Fatal("interrupted write retained")
	}
	for _, name := range []string{"uncertain", "live"} {
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Fatal(name, "removed")
		}
	}
}

func TestCommandStateConcurrentPreparation(t *testing.T) {
	baseOptions := commandStateOptions(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			o := baseOptions
			o.Workbench = &Workbench{Commands: &Commands{}}
			s := &CommandSandbox{}
			if err := prepareCommandState(context.Background(), s, &o); err != nil {
				t.Error(err)
				return
			}
			defer s.lock.Close()
			if _, err := os.Stat(s.dir); err != nil {
				t.Error("live state disappeared:", err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestCommandStateParentLockWaitHonorsCancellation(t *testing.T) {
	o := commandStateOptions(t)
	base := filepath.Join(o.RuntimeHome, "commands")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSession(base)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := prepareCommandState(ctx, &CommandSandbox{}, &o); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
