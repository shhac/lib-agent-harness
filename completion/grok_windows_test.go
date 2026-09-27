//go:build windows

package completion

import (
	"context"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

// Grok's private runtime home cannot be made owner-only here, so completion
// refuses before any process, probe or login is touched.
func TestGrokCompletionIsRefusedOnWindowsBeforeLaunch(t *testing.T) {
	cfg := Config{Provider: cliProvider(harness.Grok, "grok", ""), Model: "grok-4.7", WorkDirRoot: t.TempDir(),
		BeforeRequest: func(context.Context) error { t.Fatal("reached the request hook"); return nil },
		run: func(context.Context, string, []string, string, []string, string) ([]byte, error) {
			t.Fatal("started a process")
			return nil, nil
		}}
	_, err := Complete(context.Background(), cfg, userMessage, Tools())
	failure := requireDiagnostic(t, err, harness.Grok, PhasePreflight, "grok_platform_unsupported")
	if facts := failure.HarnessFacts(); facts.Family != harness.FailureCapability {
		t.Fatalf("%+v", facts)
	}
}
