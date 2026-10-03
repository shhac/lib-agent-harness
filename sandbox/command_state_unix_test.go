//go:build darwin || linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
)

func commandStateOptions(t *testing.T) Options {
	return Options{RuntimeHome: t.TempDir()}
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
	s := &Sandbox{}
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

			s := &Sandbox{}
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
	if err := prepareCommandState(ctx, &Sandbox{}, &o); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func legacyCommandEntries(t *testing.T, home string) (string, string, string, *os.File) {
	t.Helper()
	base := filepath.Join(home, "commands")
	stale, empty, live := filepath.Join(base, "sandbox-legacy"), filepath.Join(base, "sandbox-interrupted"), filepath.Join(base, "sandbox-live")
	for _, dir := range []string{stale, empty, live} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.lock"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// v0.22 serialized exactly these exported fields, with a 32-hex token.
	data, err := json.Marshal(struct {
		Token string
		Since time.Time
	}{process.NewToken(), time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stale, "workbench-token.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSession(live)
	if err != nil {
		t.Fatal(err)
	}
	return stale, empty, live, lock
}
func assertLegacyCommandSweep(t *testing.T, stale, empty, live string) {
	t.Helper()
	for _, dir := range []string{stale, empty} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("old entry retained: %s: %v", dir, err)
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live old entry removed", err)
	}
}
func TestCommandStateLegacyFormatSweep(t *testing.T) {
	testenv.RequireProcessStatus(t)
	o := commandStateOptions(t)
	stale, empty, live, lock := legacyCommandEntries(t, o.RuntimeHome)
	defer lock.Close()
	s := &Sandbox{}
	if err := prepareCommandState(context.Background(), s, &o); err != nil {
		t.Fatal(err)
	}
	defer s.lock.Close()
	assertLegacyCommandSweep(t, stale, empty, live)
}
func TestCommandSandboxLegacyFormatOpen(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	requireCommandPlatform(t)
	stale, empty, live, lock := legacyCommandEntries(t, opts.RuntimeHome)
	defer lock.Close()
	s := openTestCommandSandbox(t, opts)
	defer s.Close()
	assertLegacyCommandSweep(t, stale, empty, live)
}
