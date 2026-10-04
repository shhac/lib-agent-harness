package session

import (
	"context"
	"errors"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// On Windows `cmd` names the system command interpreter, and Command Code
// sessions have not been verified there, so one is refused before launch.
func TestCommandCodeSessionRefusesWindows(t *testing.T) {
	s, err := Start(context.Background(), Options{Provider: harness.Provider{Engine: harness.CommandCode}, WorkDir: t.TempDir()})
	if s != nil {
		s.Close()
		t.Fatal("a Command Code session started on Windows")
	}
	var refusal *UnsupportedError
	if !errors.As(err, &refusal) || refusal.Code != RefusedEngine || refusal.Engine != harness.CommandCode {
		t.Fatalf("expected a pre-launch engine refusal, got %v", err)
	}
}
