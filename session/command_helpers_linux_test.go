package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func requireWorkbenchBwrap(t *testing.T) (string, string) {
	t.Helper()
	testenv.RequireProcessGroup(t)
	work, home, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var binary, version string
	testenv.RequireBwrap(t, func() error {
		var e error
		binary, version, e = sandboxhook.CommandTrial(ctx, work, home, tmp)
		var cap *sandbox.ProofError
		if errors.As(e, &cap) && (cap.Code == CapabilitySandboxToolMissing || cap.Code == CapabilitySandboxToolOutdated || cap.Code == CapabilitySandboxNamespacesUnavailable) {
			return errors.Join(fs.ErrPermission, e)
		}
		return e
	})
	if os.Getenv("AGENT_HARNESS_TEST_BWRAP_080") == "1" && version != "0.8.0" {
		t.Fatalf("expected minimum bwrap, got %s", version)
	}
	return binary, version
}

func requireLegacyCommandPlatform(t *testing.T) { t.Helper(); requireWorkbenchBwrap(t) }
