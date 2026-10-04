package session

import (
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func requireWorkbenchSeatbelt(t *testing.T) {
	t.Helper()
	testenv.RequireLoopback(t) // Real command proofs need network witnesses.
	testenv.RequireProcessGroup(t)
	testenv.RequireNestedSandbox(t)
}

func requireLegacyCommandPlatform(t *testing.T) { t.Helper(); requireWorkbenchSeatbelt(t) }
