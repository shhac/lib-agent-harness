package session

import (
	"context"
	"errors"
	"testing"
)

func TestSandboxedSessionRefusesUnsupportedPlatform(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			s, err := Start(context.Background(), Options{Engine: engine, WorkDir: t.TempDir(), Home: t.TempDir(), RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Write: true}})
			if s != nil {
				s.Close()
				t.Fatal("unsupported sandboxed session started")
			}
			var failure *CapabilityError
			if !errors.As(err, &failure) || failure.Code != CapabilitySandboxUnavailable || failure.Phase != BeforeLaunch {
				t.Fatalf("expected pre-launch platform refusal, got %v", err)
			}
		})
	}
}
