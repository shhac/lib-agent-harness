package session

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

func fakeCommandSandbox(t *testing.T, execute func(context.Context, string, string, time.Duration, func()) (CommandResult, error)) *CommandSandbox {
	t.Helper()
	w, err := openWorkspace(Options{WorkDir: t.TempDir(), Workbench: &Workbench{}, Restriction: &Restriction{Tools: ToolHost{}}})
	if err != nil {
		t.Fatal(err)
	}
	w.commands = &commandSandbox{timeout: time.Second, execute: execute, close: func() error { return nil }}
	s := &CommandSandbox{ws: w, active: make(map[*StartedCommand]struct{}), dir: t.TempDir()}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func requireCommandCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *TurnError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestCommandSandboxLifecycle(t *testing.T) {
	s := fakeCommandSandbox(t, func(ctx context.Context, command, dir string, timeout time.Duration, onStart func()) (CommandResult, error) {
		notifyCommandLaunch(onStart)
		if command == "exit" {
			return CommandResult{ExitCode: 7, Stdout: dir, Stderr: "err", Truncated: true}, nil
		}
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	})
	r, err := s.Run(context.Background(), CommandRequest{Command: "exit"})
	if err != nil || r.ExitCode != 7 || r.Stdout != "." || r.Stderr != "err" || !r.Truncated {
		t.Fatalf("%+v %v", r, err)
	}
	h, err := s.Start(context.Background(), CommandRequest{Command: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	h.Stop()
	h.Stop()
	if _, err := h.Result(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	finished, err := s.Start(context.Background(), CommandRequest{Command: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := finished.Result(); err != nil || r.ExitCode != 7 {
		t.Fatalf("%+v %v", r, err)
	}
	finished.Stop()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.Run(context.Background(), CommandRequest{Command: "exit"})
	requireCommandCode(t, err, CommandSandboxClosed)
}

func TestCommandSandboxParallelClose(t *testing.T) {
	launched := make(chan struct{}, 64)
	s := fakeCommandSandbox(t, func(ctx context.Context, command, dir string, timeout time.Duration, onStart func()) (CommandResult, error) {
		notifyCommandLaunch(onStart)
		launched <- struct{}{}
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	})
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Run(context.Background(), CommandRequest{Command: "wait"})
			requireCommandCode(t, err, CommandSandboxClosed)
		}()
	}
	// Exercise Close against admitted work as well as callers still entering.
	<-launched
	<-launched
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestCommandSandboxValidationAndLimit(t *testing.T) {
	s := fakeCommandSandbox(t, func(ctx context.Context, command, dir string, timeout time.Duration, onStart func()) (CommandResult, error) {
		notifyCommandLaunch(onStart)
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	})
	for _, req := range []CommandRequest{{}, {Command: "a\x00b"}, {Command: "wait", Timeout: 2 * time.Second}, {Command: "wait", Dir: "../outside"}} {
		if _, err := s.Run(context.Background(), req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	if _, err := s.Start(context.Background(), CommandRequest{Command: "wait", Timeout: time.Second}); err == nil {
		t.Fatal("Start accepted timeout")
	}
	missing := CommandRequest{Command: "wait", Dir: "missing"}
	_, dirErr := s.Run(context.Background(), missing)
	var failure *TurnError
	if !errors.As(dirErr, &failure) || failure.Code == CommandProcessLimit {
		t.Fatalf("expected directory refusal, got %v", dirErr)
	}
	for range 64 {
		if _, err := s.Start(context.Background(), CommandRequest{Command: "wait"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Run(context.Background(), missing)
	requireCommandCode(t, err, failure.Code)
	_, err = s.Start(context.Background(), missing)
	requireCommandCode(t, err, failure.Code)
	_, err = s.Start(context.Background(), CommandRequest{Command: "wait"})
	requireCommandCode(t, err, CommandProcessLimit)
}

func TestCommandSandboxHostedResultContract(t *testing.T) {
	s := &commandSandbox{execute: func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{ExitCode: 3, Stdout: "out", Stderr: "err"}, nil
	}}
	r, err := s.run(context.Background(), "", ".", time.Second)
	if err != nil || r.Content != `{"exit_code":3,"stdout":"out","stderr":"err","timed_out":false,"truncated":false}` {
		t.Fatalf("%+v %v", r, err)
	}
	s.execute = func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{}, commandError(CommandOutcomeUnknown)
	}
	r, err = s.run(context.Background(), "", ".", time.Second)
	if !errors.Is(err, errWorkbenchCommandUnknown) || !r.IsError || r.Content != "run_command error: command_outcome_unknown: \".\"" {
		t.Fatalf("%+v %v", r, err)
	}
	s.execute = func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{}, commandError(CommandSandboxClosed)
	}
	r, err = s.run(context.Background(), "", ".", time.Second)
	if !errors.Is(err, context.Canceled) || r.IsError || r.Content != "" {
		t.Fatalf("hosted closure contract changed: %+v %v", r, err)
	}
}

func TestCommandSandboxCloseDoesNotWaitForDirectoryValidation(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "Run", true: "Start"}[start], func(t *testing.T) {
			s := fakeCommandSandbox(t, func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
				t.Error("command launched after Close")
				return CommandResult{}, nil
			})
			if err := s.ws.root.Mkdir("nested", 0700); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseWalk := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseWalk()
			s.ws.openStep = func() { close(entered); <-release }
			result := make(chan error, 1)
			go func() {
				req := CommandRequest{Command: "wait", Dir: "nested"}
				if start {
					_, err := s.Start(context.Background(), req)
					result <- err
				} else {
					_, err := s.Run(context.Background(), req)
					result <- err
				}
			}()
			<-entered
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close blocked behind directory walk")
			}
			releaseWalk()
			requireCommandCode(t, <-result, CommandSandboxClosed)
		})
	}
}

func TestCommandSandboxCallerCancellationWinsDuringClose(t *testing.T) {
	s := fakeCommandSandbox(t, func(ctx context.Context, command, dir string, timeout time.Duration, onStart func()) (CommandResult, error) {
		notifyCommandLaunch(onStart)
		<-ctx.Done()
		return CommandResult{}, commandError(CommandStartFailed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	h, err := s.Start(ctx, CommandRequest{Command: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Result(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCommandSandboxCloseBeforeLaunch(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "Run", true: "Start"}[start], func(t *testing.T) {
			entered := make(chan struct{})
			s := fakeCommandSandbox(t, func(ctx context.Context, command, dir string, timeout time.Duration, onStart func()) (CommandResult, error) {
				close(entered)
				<-ctx.Done()
				return CommandResult{}, commandError(CommandStartFailed)
			})
			result := make(chan error, 1)
			go func() {
				if start {
					_, err := s.Start(context.Background(), CommandRequest{Command: "wait"})
					result <- err
				} else {
					_, err := s.Run(context.Background(), CommandRequest{Command: "wait"})
					result <- err
				}
			}()
			<-entered
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			requireCommandCode(t, <-result, CommandSandboxClosed)
		})
	}
}

func TestCommandSandboxPlatformRefusal(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		return
	}
	_, err := OpenCommandSandbox(context.Background(), CommandSandboxOptions{})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Operation != "sandbox" || unsupported.Capability != harness.Support(harness.OpenAICompatible, harness.Session, harness.Sandbox) {
		t.Fatal(err)
	}
}

func TestCommandSandboxLinuxStartLoopbackRefusal(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	s := &CommandSandbox{loopback: true}
	_, err := s.Start(context.Background(), CommandRequest{Command: "sleep 30"})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Operation != "start" || unsupported.Capability.Availability != harness.Unsupported {
		t.Fatal(err)
	}
}

func TestCommandSandboxCleanupFailurePreservesState(t *testing.T) {
	dir := t.TempDir()
	lock, err := lockSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	failure := commandError(CommandCleanupUnknown)
	w := &workspace{root: root, stop: make(chan struct{}), commands: &commandSandbox{close: func() error { return failure }}}
	s := &CommandSandbox{ws: w, dir: dir, lock: lock}
	if s.Close() != failure || s.Close() != failure {
		t.Fatal("cleanup failure not stable")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("uncertain state deleted")
	}
	secondLock, err := lockSession(dir)
	if err != nil {
		t.Fatal("lock retained on failed cleanup:", err)
	}
	secondLock.Close()
}
