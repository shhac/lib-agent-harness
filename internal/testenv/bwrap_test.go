package testenv

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestBwrapInterruptionNeverSkips(t *testing.T) {
	for _, strict := range []string{"0", "1"} {
		t.Setenv(NoSkipVariable, strict)
		for _, stage := range []string{"before", "during", "deadline"} {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			if stage == "before" {
				cancel()
			}
			if stage == "deadline" {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			called := false
			r := &recorder{}
			func() {
				defer func() {
					if v := recover(); v != nil && v != r {
						panic(v)
					}
				}()
				RequireBwrap(r, ctx, func() error {
					called = true
					cancel()
					return errors.Join(fs.ErrPermission, errors.New("namespace trial refused"))
				})
			}()
			if called != (stage == "during") || r.skipped != "" || !strings.Contains(r.failed, "probing bubblewrap") || !strings.Contains(r.failed, want.Error()) || strings.Contains(r.failed, "environment refuses") {
				t.Fatalf("strict=%s stage=%s called=%v skip=%q fail=%q", strict, stage, called, r.skipped, r.failed)
			}
		}
	}
}
