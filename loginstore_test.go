package harness_test

import (
	"context"
	"runtime"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/account"
	"github.com/shhac/lib-agent-harness/catalog"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/loginstore"
	"github.com/shhac/lib-agent-harness/native"
	"github.com/shhac/lib-agent-harness/session"
	keyring "github.com/shhac/lib-agent-keyring"
)

// With the keychain locked, every operation refuses Claude before launching
// anything. The binary does not exist, so an operation that skipped the check
// would fail differently.
func TestALockedKeychainStopsEveryClaudeLaunch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only macOS keeps Claude's login in a keychain")
	}
	loginstore.Status = func() keyring.Status { return keyring.Locked }
	t.Cleanup(func() { loginstore.Status = keyring.HostStatus })
	if !harness.LoginStoreLocked(harness.Claude) || harness.LoginStoreLocked(harness.Codex) || harness.LoginStoreLocked(harness.Grok) {
		t.Fatal("only Claude's login is in the keychain")
	}
	provider := harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: "/nonexistent/claude", Home: t.TempDir()}}
	ctx := context.Background()
	work := t.TempDir()
	_, native := native.Run(ctx, native.Config{Provider: provider}, native.Request{Prompt: "p", WorkDir: work}, nil)
	_, completion := completion.Complete(ctx, completion.Config{Provider: provider, Model: "m"}, []completion.Message{{Role: "user", Content: "p"}}, nil)
	_, catalog := catalog.Discover(ctx, provider)
	_, account := account.Inspect(ctx, provider)
	_, started := session.Start(ctx, session.Options{Provider: provider, WorkDir: work})
	_, inspected := session.Inspect(ctx, session.Options{Provider: provider, WorkDir: work})
	sandbox := session.VerifySandbox(ctx, session.Options{Provider: provider, WorkDir: work, Sandbox: &session.Sandbox{}})
	for name, err := range map[string]error{"native": native, "completion": completion, "catalog": catalog, "account": account, "session": started, "inspect": inspected, "sandbox": sandbox} {
		facts, ok := harness.ErrorFacts(err)
		if !ok || facts.Code != harness.CodeKeychainUnavailable || facts.Family != harness.FailurePreflight || facts.Retryable {
			t.Errorf("%s: %+v %v", name, facts, err)
		}
	}
	loginstore.Status = func() keyring.Status { return keyring.Ready }
	if harness.LoginStoreLocked(harness.Claude) {
		t.Fatal("an unlocked keychain reported locked")
	}
}
