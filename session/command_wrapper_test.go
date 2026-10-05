package session

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func sandboxOpts(o CommandSandboxOptions) sandbox.Options {
	return sandbox.Options{WorkDir: o.WorkDir, RuntimeHome: o.RuntimeHome, Write: o.Write, Read: o.Read, Env: o.Env, Loopback: o.Loopback, Timeout: o.Timeout, Background: o.Background}
}
func TestDeprecatedCommandSandboxOpenRefusals(t *testing.T) {
	for _, kind := range []string{"platform", "runtime", "work", "env", "read", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			o := CommandSandboxOptions{WorkDir: t.TempDir(), RuntimeHome: privateHome(t)}
			switch kind {
			case "platform":
				if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
					return
				}
			case "runtime":
				o.RuntimeHome = ""
			case "work":
				o.WorkDir = "relative"
			case "env":
				o.Env = []string{"HOME=/tmp"}
			case "read":
				o.Read = []string{"relative"}
			case "timeout":
				o.Timeout = -1
			}
			_, raw := sandbox.Open(context.Background(), sandboxOpts(o))
			_, err := OpenCommandSandbox(context.Background(), o)
			if raw == nil || err == nil || !reflect.DeepEqual(fromSandbox(raw, commandSandboxTranslation), err) {
				t.Fatalf("wrapper drift: %v / %v", raw, err)
			}
			hosted := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, WorkDir: o.WorkDir, RuntimeHome: o.RuntimeHome, Restriction: &Restriction{}, Workbench: &Workbench{Write: o.Write, Commands: &Commands{Read: o.Read, Env: o.Env, Loopback: o.Loopback, Timeout: o.Timeout}}}
			var hostedErr error
			if runtime.GOOS == "windows" {
				_, hostedErr = normalizeWorkbench(hosted)
			} else {
				hosted, hostedErr = normalizeRuntimeHome(hosted)
				if hostedErr == nil {
					_, hostedErr = normalizeWorkbench(hosted)
				}
			}
			if kind == "timeout" && (runtime.GOOS == "darwin" || runtime.GOOS == "linux") {
				// Both paths refuse, but name their distinct timeout ceilings.
				if workbenchRefusal(t, hostedErr) != RefusedLimit || workbenchRefusal(t, err) != RefusedLimit ||
					!strings.Contains(hostedErr.Error(), "ten minutes") || !strings.Contains(err.Error(), "two hours") {
					t.Fatalf("wrong timeout ceilings: %v / %v", hostedErr, err)
				}
			} else if !reflect.DeepEqual(hostedErr, err) {
				t.Fatalf("hosted/standalone refusal drift: %v / %v", hostedErr, err)
			}
			want, wok := harness.ErrorFacts(raw)
			got, gok := harness.ErrorFacts(err)
			if !wok || !gok || !reflect.DeepEqual(want, got) || !errors.Is(err, ErrUnsupported) {
				t.Fatalf("facts drift: %+v / %+v", want, got)
			}
		})
	}
}
func fakeWrappedCommands(t *testing.T, code string) (*sandbox.Sandbox, *CommandSandbox) {
	t.Helper()
	files, err := sandbox.OpenWorkspace(sandbox.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := &sandbox.Runner{}
	sandboxhook.RunnerAccess(r).SetExecute(func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
		return CommandResult{}, &sandbox.CommandError{Code: code}
	})
	*sandboxhook.RunnerAccess(r).Close = func() error {
		if code == CommandCleanupUnknown {
			return &sandbox.CommandError{Code: code}
		}
		return nil
	}
	s := sandboxhook.NewCommandSandbox(files, r, t.TempDir(), false).(*sandbox.Sandbox)
	return s, &CommandSandbox{inner: s}
}
func TestDeprecatedCommandSandboxCommandErrors(t *testing.T) {
	for _, code := range []string{CommandStartFailed, CommandOutcomeUnknown, CommandProcessLimit, CommandCleanupUnknown, CommandSandboxClosed} {
		t.Run(code, func(t *testing.T) {
			raw, wrapper := fakeWrappedCommands(t, code)
			defer raw.Close()
			if code == CommandSandboxClosed {
				if err := wrapper.Close(); err != nil {
					t.Fatal(err)
				}
			}
			compare := func(a, b error) {
				t.Helper()
				if !reflect.DeepEqual(fromSandbox(a, commandSandboxTranslation), b) || !errors.Is(b, ErrTurnFailed) {
					t.Fatalf("wrapper drift: %v / %v", a, b)
				}
			}
			_, a := raw.Run(context.Background(), sandbox.CommandRequest{Command: "true"})
			_, b := wrapper.Run(context.Background(), CommandRequest{Command: "true"})
			compare(a, b)
			_, a = raw.Start(context.Background(), sandbox.CommandRequest{Command: "true"})
			_, b = wrapper.Start(context.Background(), CommandRequest{Command: "true"})
			compare(a, b)
			if code == CommandCleanupUnknown {
				a = raw.Close()
				b = wrapper.Close()
				compare(a, b)
				compare(a, wrapper.Close())
				if wrapper.Close() != b {
					t.Fatal("translated Close error identity changed")
				}
			} else {
				wrapper.Close()
			}
		})
	}
}
func TestDeprecatedCommandSandboxSettledHandle(t *testing.T) {
	files, err := sandbox.OpenWorkspace(sandbox.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := &sandbox.Runner{}
	sandboxhook.RunnerAccess(r).SetExecute(func(ctx context.Context, _ string, _ string, _ time.Duration, onStart func()) (CommandResult, error) {
		onStart()
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	})
	*sandboxhook.RunnerAccess(r).Close = func() error { return nil }
	s := &CommandSandbox{inner: sandboxhook.NewCommandSandbox(files, r, t.TempDir(), false).(*sandbox.Sandbox)}
	h, err := s.Start(context.Background(), CommandRequest{Command: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	<-h.Done()
	if _, err = h.Result(); !errors.Is(err, ErrTurnFailed) {
		t.Fatal(err)
	}
	h.Stop()
}

func TestDeprecatedCommandSandboxLinuxStartLoopbackRefusal(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	files, err := sandbox.OpenWorkspace(sandbox.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := &sandbox.Runner{}
	*sandboxhook.RunnerAccess(r).Close = func() error { return nil }
	raw := sandboxhook.NewCommandSandbox(files, r, t.TempDir(), true).(*sandbox.Sandbox)
	defer raw.Close()
	wrapper := &CommandSandbox{inner: raw}
	_, a := raw.Start(context.Background(), sandbox.CommandRequest{Command: "sleep 30"})
	_, b := wrapper.Start(context.Background(), CommandRequest{Command: "sleep 30"})
	var refused *UnsupportedError
	if !errors.As(b, &refused) || refused.Operation != "start" || refused.Code != RefusedNotOffered || !reflect.DeepEqual(fromSandbox(a, commandSandboxTranslation), b) {
		t.Fatalf("Linux wrapper refusal drift: %v / %v", a, b)
	}
	wrapper.Close()
	_, a = raw.Start(context.Background(), sandbox.CommandRequest{Command: "sleep 30"})
	_, b = wrapper.Start(context.Background(), CommandRequest{Command: "sleep 30"})
	if !reflect.DeepEqual(fromSandbox(a, commandSandboxTranslation), b) || !errors.Is(b, ErrTurnFailed) {
		t.Fatalf("closed must precede loopback refusal: %v / %v", a, b)
	}
}

func TestDeprecatedStartFailureRetainsDiagnostics(t *testing.T) {
	for _, cause := range []string{"cancel", "close"} {
		t.Run(cause, func(t *testing.T) {
			files, err := sandbox.OpenWorkspace(sandbox.Config{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			want := CommandResult{ExitCode: -1, Stderr: "[harness PATH: dropped unreadable]\n", Truncated: true}
			r := &sandbox.Runner{}
			sandboxhook.RunnerAccess(r).SetExecute(func(runCtx context.Context, _, _ string, _ time.Duration, onStart func()) (CommandResult, error) {
				close(entered)
				if cause == "cancel" {
					cancel()
				} else {
					<-runCtx.Done()
				}
				onStart()
				return want, &sandbox.CommandError{Code: CommandOutcomeUnknown}
			})
			*sandboxhook.RunnerAccess(r).Close = func() error { return nil }
			s := &CommandSandbox{inner: sandboxhook.NewCommandSandbox(files, r, t.TempDir(), false).(*sandbox.Sandbox)}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			closed := make(chan error, 1)
			if cause == "close" {
				go func() { <-entered; closed <- s.Close() }()
			}
			h, err := s.Start(ctx, CommandRequest{Command: "wait"})
			if h == nil || err == nil {
				t.Fatalf("lost handle: %v %v", h, err)
			}
			if cause == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				var failure *TurnError
				if !errors.As(err, &failure) || failure.Code != CommandSandboxClosed {
					t.Fatal(err)
				}
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				got, resultErr := h.Result()
				if got != want || !reflect.DeepEqual(resultErr, err) {
					t.Fatalf("%+v %v / %v", got, resultErr, err)
				}
			}
			h.Stop()
		})
	}
}

func TestDeprecatedCommandRunDiagnosticsBudgets(t *testing.T) {
	for _, budget := range []int{minWorkbenchResult, maxWorkbenchResult} {
		for _, cause := range []string{"success", "generic", "unknown", "cancel", "deadline", "closed"} {
			t.Run(cause+"/"+strconv.Itoa(budget), func(t *testing.T) {
				files, err := sandbox.OpenWorkspace(sandbox.Config{Root: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				runner := &sandbox.Runner{}
				want := CommandResult{Stderr: "[harness PATH: dropped unreadable]\n"}
				var rawErr error
				switch cause {
				case "generic":
					rawErr = errors.New("generic")
				case "unknown":
					rawErr = &sandbox.CommandError{Code: CommandOutcomeUnknown}
				case "cancel":
					rawErr = context.Canceled
				case "deadline":
					rawErr = context.DeadlineExceeded
				case "closed":
					rawErr = &sandbox.CommandError{Code: CommandSandboxClosed}
				}
				sandboxhook.RunnerAccess(runner).SetExecute(func(context.Context, string, string, time.Duration, func()) (CommandResult, error) {
					return want, rawErr
				})
				*sandboxhook.RunnerAccess(runner).Close = func() error { return nil }
				wrapper := &CommandSandbox{inner: sandboxhook.NewCommandSandbox(files, runner, t.TempDir(), false).(*sandbox.Sandbox)}
				defer wrapper.Close()
				got, err := wrapper.Run(context.Background(), CommandRequest{Command: "true"})
				if got != want || len(got.Stderr) > (budget-1024)/12 || strings.Count(got.Stderr, "[harness PATH:") != 1 || !reflect.DeepEqual(err, fromSandbox(rawErr, commandSandboxTranslation)) {
					t.Fatalf("diagnostics/translation changed: %+v %v", got, err)
				}
			})
		}
	}
}
