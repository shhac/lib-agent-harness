package session

import (
	"context"
	"runtime"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
)

func TestCommandAndCLIVerificationCachesAreSeparate(t *testing.T) {
	prior := verified
	verified = &verificationCache{seen: map[string]bool{}}
	defer func() { verified = prior }()
	sandboxhook.ResetCommandCache()
	defer sandboxhook.ResetCommandCache()
	verified.record("cli")
	if sandboxhook.HasCommandProof("cli") {
		t.Fatal("CLI verification reached command cache")
	}
	sandboxhook.RecordCommandProof("command")
	if verified.holds("command") {
		t.Fatal("command verification reached CLI cache")
	}
	if runtime.GOOS == "windows" {
		return
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{}
	count := sandboxhook.CommandCacheSize()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := proveWorkbench(ctx, o); err == nil {
		t.Fatal("cancelled proof succeeded")
	}
	if sandboxhook.CommandCacheSize() != count || len(verified.seen) != 1 || !verified.holds("cli") {
		t.Fatal("failed proof changed verification caches")
	}
}
