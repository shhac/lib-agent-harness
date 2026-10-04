package testenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// recorder is a testing.TB that records how a helper ended the test instead
// of ending it. Skip and Fatal stop the calling goroutine, as the real ones do.
type recorder struct {
	testing.TB
	skipped, failed string
}

func (r *recorder) Helper() {}
func (r *recorder) Skip(args ...any) {
	r.skipped = fmt.Sprint(args...)
	panic(r)
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
	panic(r)
}

func run(err error) (r *recorder) {
	r = &recorder{}
	defer func() {
		if v := recover(); v != nil && v != r {
			panic(v)
		}
	}()
	require(r, "a thing", err)
	return r
}

func TestRequireSkipsOnlyARefusal(t *testing.T) {
	t.Setenv(NoSkipVariable, "")
	for _, tc := range []struct {
		name          string
		err           error
		skip, failure string
	}{
		{name: "allowed"},
		{name: "eperm", err: &Refusal{Op: "listen", Err: syscall.EPERM}, skip: "environment refuses a thing: listen: operation not permitted"},
		{name: "eacces", err: &Refusal{Op: "connect", Err: syscall.EACCES}, skip: "environment refuses a thing: connect: permission denied"},
		{name: "wrapped", err: fmt.Errorf("start: %w", &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.EPERM}), skip: "environment refuses a thing"},
		{name: "enoent", err: &Refusal{Op: "start", Err: syscall.ENOENT}, failure: "probing a thing failed: start: " + syscall.ENOENT.Error()},
		{name: "socket limit", err: &Refusal{Op: "socket path", Err: ErrSocketPathTooLong}, skip: "environment refuses a thing: socket path: "},
		{name: "einval", err: &Refusal{Op: "listen", Err: syscall.EINVAL}, failure: "probing a thing failed: listen: " + syscall.EINVAL.Error()},
		{name: "other", err: errors.New("ps reported nothing"), failure: "probing a thing failed: ps reported nothing"},
		{name: "deadline", err: context.DeadlineExceeded, failure: "probing a thing failed: context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(tc.err)
			if !strings.HasPrefix(r.skipped, tc.skip) || (tc.skip == "") != (r.skipped == "") {
				t.Errorf("skip %q, want %q", r.skipped, tc.skip)
			}
			if r.failed != tc.failure {
				t.Errorf("failure %q, want %q", r.failed, tc.failure)
			}
		})
	}
}

func TestNoSkipTurnsARefusalIntoAFailure(t *testing.T) {
	t.Setenv(NoSkipVariable, "1")
	r := run(&Refusal{Op: "listen", Err: syscall.EPERM})
	if r.skipped != "" || !strings.Contains(r.failed, "environment refuses a thing: listen: operation not permitted") || !strings.Contains(r.failed, NoSkipVariable+"=1") {
		t.Fatalf("skipped %q failed %q", r.skipped, r.failed)
	}
	if r = run(nil); r.skipped != "" || r.failed != "" {
		t.Fatalf("an allowed probe ended the test: skipped %q failed %q", r.skipped, r.failed)
	}
}

func TestJoinedRefusalIsOneLine(t *testing.T) {
	err := errors.Join(syscall.EPERM, errors.New("namespace trial\nadditional diagnostic"))
	for _, strict := range []string{"", "1"} {
		t.Setenv(NoSkipVariable, strict)
		r := run(err)
		message := r.skipped + r.failed
		if strings.ContainsAny(message, "\r\n\t") || !strings.Contains(message, "environment refuses a thing: operation not permitted namespace trial additional diagnostic") {
			t.Fatalf("lost one-line diagnostic: %q", message)
		}
		if strict == "1" && (r.skipped != "" || !strings.Contains(r.failed, "forbids skipping")) {
			t.Fatalf("%+v", r)
		}
	}
}

func TestProbeRunsOnceForEveryCaller(t *testing.T) {
	var calls atomic.Int32
	p := &probe{check: func() error {
		calls.Add(1)
		return &Refusal{Op: "listen", Err: syscall.EPERM}
	}}
	var wg sync.WaitGroup
	verdicts := make([]error, 16)
	for i := range verdicts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i] = p.result()
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("probed %d times", calls.Load())
	}
	for _, v := range verdicts {
		if v != verdicts[0] {
			t.Fatalf("callers saw different verdicts: %v and %v", v, verdicts[0])
		}
	}
}

// The real probes either pass, or report a refusal, here; which one depends on
// where the suite runs, and the Require helpers act on it. Nothing else is an
// acceptable outcome, and the verdict is logged for go test -v.
func TestProbesReportAllowedOrRefused(t *testing.T) {
	for name, p := range map[string]*probe{"unix socket": unixSocket, "process group": processGroup, "process status": processStatus, "group priority": groupPriority, "nested OS sandbox": nestedSandbox, "loopback listener": loopback, "atomic workbench temporary": atomicWrite} {
		err := p.result()
		t.Logf("%s: %v", name, err)
		if err != nil && !Refused(err) {
			t.Errorf("%s probe failed without a refusal: %v", name, err)
		}
	}
}

func TestNewCapabilityReasonsAndNoSkip(t *testing.T) {
	for _, what := range []string{"a nested OS sandbox", "a loopback listener", "an explicit-port loopback bind", "a Unix domain socket under TMPDIR"} {
		for _, noSkip := range []string{"", "1"} {
			t.Setenv(NoSkipVariable, noSkip)
			r := &recorder{}
			func() {
				defer func() {
					if v := recover(); v != nil && v != r {
						panic(v)
					}
				}()
				require(r, what, &Refusal{Op: "probe", Err: ErrSocketPathTooLong})
			}()
			if noSkip == "" {
				if !strings.Contains(r.skipped, what) || !strings.Contains(r.skipped, "90-byte") || r.failed != "" {
					t.Fatalf("%+v", r)
				}
			} else if r.skipped != "" || !strings.Contains(r.failed, "forbids skipping") || !strings.Contains(r.failed, what) {
				t.Fatalf("%+v", r)
			}
		}
	}
}

func TestCapabilityProbesShareVerdict(t *testing.T) {
	for _, p := range []*probe{unixSocket, processGroup, processStatus, groupPriority, nestedSandbox, loopback, atomicWrite} {
		want := p.result()
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				if got := p.result(); got != want {
					t.Errorf("verdict changed: %v / %v", got, want)
				}
			})
		}
		wg.Wait()
	}
}
