package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCommandTimeoutLimits(t *testing.T) {
	if MaxStandaloneTimeout < 45*time.Minute || MaxSessionTimeout != 10*time.Minute {
		t.Fatal("unexpected timeout ceilings")
	}
	for _, standalone := range []bool{false, true} {
		limit, reason := MaxSessionTimeout, "ten minutes"
		if standalone {
			limit, reason = MaxStandaloneTimeout, "two hours"
		}
		for _, timeout := range []time.Duration{0, -1, 10 * time.Minute, 10*time.Minute + 1, 11 * time.Minute, 45 * time.Minute, limit, limit + 1} {
			t.Run(time.Duration(timeout).String()+"/standalone="+map[bool]string{true: "true", false: "false"}[standalone], func(t *testing.T) {
				o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Timeout: timeout}
				if err := os.Chmod(o.RuntimeHome, 0700); err != nil {
					t.Fatal(err)
				}
				n, err := normalize(o, standalone)
				if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
					var refused *RefusalError
					if !errors.As(err, &refused) || refused.Code != RefusedNotOffered {
						t.Fatal(err)
					}
					return
				}
				if timeout < 0 || timeout > limit {
					var refused *RefusalError
					if !errors.As(err, &refused) || refused.Code != RefusedLimit || !strings.Contains(refused.Capability.Reason, reason) {
						t.Fatalf("limit refusal: %v", err)
					}
					return
				}
				want := timeout
				if want == 0 {
					want = 2 * time.Minute
				}
				if err != nil || n.Timeout != want {
					t.Fatalf("timeout %v: %v", n.Timeout, err)
				}
			})
		}
	}
}

func TestHostedRunnerTimeoutRefusesBeforeState(t *testing.T) {
	o := Options{Timeout: 45 * time.Minute}
	p := Proof{options: o, system: []string{"/proved-system"}}
	dir := filepath.Join(t.TempDir(), "absent")
	r, err := NewRunner(o, p, dir, MinResult)
	var refused *RefusalError
	if r != nil || !errors.As(err, &refused) || refused.Code != RefusedLimit || refused.Capability.Reason != "hosted command timeout must be between zero and ten minutes" {
		t.Fatalf("hosted timeout admitted: %v %v", r, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("refusal created state", err)
	}
}
