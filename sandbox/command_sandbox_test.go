package sandbox

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
)

func fakeCommandSandbox(t *testing.T, execute func(context.Context, string, string, time.Duration, func()) (CommandResult, error)) *Sandbox {
	t.Helper()
	w, err := OpenWorkspace(Config{Root: t.TempDir(), SessionID: newID()})
	if err != nil {
		t.Fatal(err)
	}
	commands := &commandSandbox{timeout: time.Second, execute: execute, close: func() error { return nil }}
	s := &Sandbox{ws: w, commands: commands, active: make(map[*StartedCommand]struct{}), dir: t.TempDir()}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func requireCommandCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *CommandError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestCommandSandboxRequestTimeoutBound(t *testing.T) {
	for _, limit := range []time.Duration{2 * time.Minute, 45 * time.Minute} {
		t.Run(limit.String(), func(t *testing.T) {
			s := fakeCommandSandbox(t, func(_ context.Context, _, _ string, timeout time.Duration, onStart func()) (CommandResult, error) {
				notifyCommandLaunch(onStart)
				return CommandResult{Stdout: timeout.String()}, nil
			})
			s.commands.timeout = limit
			short := time.Minute
			if limit == 45*time.Minute {
				short = 30 * time.Minute
			}
			for _, timeout := range []time.Duration{0, short, limit} {
				got, err := s.Run(context.Background(), CommandRequest{Command: "fixture", Timeout: timeout})
				want := timeout
				if want == 0 {
					want = limit
				}
				if err != nil || got.Stdout != want.String() {
					t.Fatalf("%+v %v", got, err)
				}
			}
			_, err := s.Run(context.Background(), CommandRequest{Command: "fixture", Timeout: limit + time.Minute})
			requireCommandCode(t, err, ArgumentsInvalid)
		})
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

func TestStartFailureRetainsSettledDiagnostics(t *testing.T) {
	for _, cause := range []string{"cancel", "close"} {
		t.Run(cause, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			want := CommandResult{ExitCode: -1, Stderr: "[harness PATH: dropped unreadable]\n", Truncated: true}
			s := fakeCommandSandbox(t, func(runCtx context.Context, _, _ string, _ time.Duration, onStart func()) (CommandResult, error) {
				close(entered)
				if cause == "cancel" {
					cancel()
				} else {
					<-runCtx.Done() // Close is already cancelling admitted work.
				}
				notifyCommandLaunch(onStart) // Launch raced with cancellation.
				return want, commandError(CommandOutcomeUnknown)
			})
			closed := make(chan error, 1)
			if cause == "close" {
				go func() { <-entered; closed <- s.Close() }()
			}
			h, err := s.Start(ctx, CommandRequest{Command: "wait"})
			if h == nil || err == nil {
				t.Fatalf("lost post-launch handle: %v %v", h, err)
			}
			if cause == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				requireCommandCode(t, err, CommandSandboxClosed)
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				got, resultErr := h.Result()
				if got != want || resultErr == nil {
					t.Fatalf("%+v %v", got, resultErr)
				}
			}
			h.Stop()
		})
	}
}

func TestStartSettledFailureLaunchContract(t *testing.T) {
	for _, launched := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre-launch", true: "post-launch"}[launched], func(t *testing.T) {
			failure := commandError(CommandOutcomeUnknown)
			want := CommandResult{Stderr: "[harness PATH: dropped unreadable]\n"}
			h := &StartedCommand{cancel: func() {}, done: make(chan struct{}), launched: make(chan struct{}), result: want, err: failure}
			if launched {
				close(h.launched)
			}
			close(h.done)
			got, err := startFailure(h, failure)
			if err != failure || (got != nil) != launched {
				t.Fatalf("%v %v", got, err)
			}
			if got != nil {
				r, e := got.Result()
				if r != want || e != failure {
					t.Fatalf("%+v %v", r, e)
				}
			}
		})
	}
}

func TestStartExecutionFailureRetainsDiagnostics(t *testing.T) {
	want := CommandResult{ExitCode: -1, Stderr: "[harness PATH: dropped unreadable]\n", Truncated: true}
	s := fakeCommandSandbox(t, func(_ context.Context, _, _ string, _ time.Duration, onStart func()) (CommandResult, error) {
		notifyCommandLaunch(onStart)
		return want, commandError(CommandOutcomeUnknown)
	})
	h, err := s.Start(context.Background(), CommandRequest{Command: "fail"})
	if h == nil {
		t.Fatalf("lost launched handle: %v", err)
	}
	// Start may observe launch or settlement first; both retain the same handle.
	if err != nil {
		requireCommandCode(t, err, CommandOutcomeUnknown)
	}
	for range 2 {
		got, err := h.Result()
		if got != want {
			t.Fatal(got)
		}
		requireCommandCode(t, err, CommandOutcomeUnknown)
	}
	h.Stop()
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
	var failure *CommandError
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

func TestCommandSandboxCloseDoesNotWaitForDirectoryValidation(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "Run", true: "Start"}[start], func(t *testing.T) {
			s := fakeCommandSandbox(t, func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
				t.Error("command launched after Close")
				return CommandResult{}, nil
			})
			if err := sandboxhook.Access(s.ws).Root.Mkdir("nested", 0700); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseWalk := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseWalk()
			*sandboxhook.Access(s.ws).OpenStep = func() { close(entered); <-release }
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
	_, err := Open(context.Background(), Options{})
	var unsupported *RefusalError
	if !errors.As(err, &unsupported) || unsupported.Operation != "sandbox" || unsupported.Capability != harness.Support(harness.OpenAICompatible, harness.Session, harness.Sandbox) {
		t.Fatal(err)
	}
}

func TestCommandSandboxLinuxStartLoopbackRefusal(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	s := &Sandbox{loopback: true}
	_, err := s.Start(context.Background(), CommandRequest{Command: "sleep 30"})
	var unsupported *RefusalError
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
	files, err := OpenWorkspace(Config{Root: t.TempDir()})
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	failure := commandError(CommandCleanupUnknown)
	commands := &commandSandbox{close: func() error { return failure }}
	s := &Sandbox{ws: files, commands: commands, dir: dir, lock: lock}
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
