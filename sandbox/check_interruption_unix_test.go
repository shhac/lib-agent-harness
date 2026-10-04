//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// Capture helper termination without turning its intentionally failing probe
// into a failure of the regression test itself.
type prerequisiteRecorder struct {
	testing.TB
	skipped, failed string
}

func (r *prerequisiteRecorder) Helper()          {}
func (r *prerequisiteRecorder) Skip(args ...any) { r.skipped = fmt.Sprint(args...); panic(r) }
func (r *prerequisiteRecorder) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
	panic(r)
}

func TestBwrapInterruptedPrerequisiteTrials(t *testing.T) {
	testenv.RequireProcessGroup(t)
	for _, stage := range []string{"before", "version", "trial"} {
		t.Run(stage, func(t *testing.T) {
			l := linuxTestLayout(t)
			path := t.TempDir()
			marker := filepath.Join(path, "ready")
			block := "echo ready > " + workbenchShellQuote(marker) + "\nexec /bin/sleep 30\n"
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then\n"
			if stage == "version" {
				script += block
			} else {
				script += "echo 'bubblewrap 0.9.0'\nexit 0\n"
			}
			script += "fi\n" + block
			if err := os.WriteFile(filepath.Join(path, "bwrap"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", path)
			// Intentionally permit skips: interruption must nevertheless fail.
			t.Setenv(testenv.NoSkipVariable, "0")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			observed := make(chan error, 1)
			if stage == "before" {
				cancel()
				observed <- nil
			} else {
				go func() {
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						if _, err := os.Stat(marker); err == nil {
							cancel()
							observed <- nil
							return
						} else if !os.IsNotExist(err) {
							cancel()
							observed <- err
							return
						}
						select {
						case <-ctx.Done():
							observed <- fmt.Errorf("fixture never reached %s: %w", stage, ctx.Err())
							return
						case <-ticker.C:
						}
					}
				}()
			}
			r := &prerequisiteRecorder{}
			var trialErr error
			func() {
				defer func() {
					if v := recover(); v != nil && v != r {
						panic(v)
					}
				}()
				testenv.RequireBwrap(r, ctx, func() error {
					_, _, trialErr = checkBwrap(ctx, l)
					// Simulate the historical callback that classified an interrupted
					// CLI's fixed code as an environmental refusal.
					var p *ProofError
					if errors.As(trialErr, &p) && (p.Code == CapabilitySandboxToolOutdated || p.Code == CapabilitySandboxNamespacesUnavailable) {
						return errors.Join(fs.ErrPermission, trialErr)
					}
					return trialErr
				})
			}()
			if err := <-observed; err != nil {
				t.Fatal(err)
			}
			if stage != "before" {
				var p *ProofError
				want := CapabilitySandboxToolOutdated
				if stage == "trial" {
					want = CapabilitySandboxNamespacesUnavailable
				}
				if !errors.As(trialErr, &p) || p.Code != want {
					t.Fatalf("fixture lost interrupted CLI code: %v", trialErr)
				}
			}
			if r.skipped != "" || !strings.Contains(r.failed, context.Canceled.Error()) || strings.Contains(r.failed, "environment refuses") {
				t.Fatalf("interrupted %s trial skipped: %+v", stage, r)
			}
		})
	}
}
