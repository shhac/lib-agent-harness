package session

import (
	"context"
	"errors"
	"testing"
)

func TestRestrictedSessionRefusesUnsupportedPlatform(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			s, err := Start(context.Background(), Options{Engine: engine, Model: "synthetic", WorkDir: t.TempDir(), Home: t.TempDir(), Restriction: &Restriction{}})
			if s != nil {
				s.Close()
				t.Fatal("unsupported restricted session started")
			}
			var failure *CapabilityError
			if !errors.As(err, &failure) || failure.Code != CapabilityUnsupportedPlatform || failure.Phase != BeforeLaunch {
				t.Fatalf("expected pre-launch platform refusal, got %v", err)
			}
		})
	}
}
