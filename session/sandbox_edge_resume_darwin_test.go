package session

import (
	"context"
	"errors"
	harness "github.com/shhac/lib-agent-harness"
	"os"
	"testing"
)

func TestEdgeResumeMismatchBeforeProbe(t *testing.T) {
	old := edgeProbe
	defer func() { edgeProbe = old }()
	calls := 0
	edgeProbe = func(context.Context, Options) error { calls++; return nil }
	binary, _ := fakeHarness(t, "edge-session")
	base := sandboxOptions(t, harness.Codex, binary, true)
	base.RuntimeHome = t.TempDir()
	if err := os.Chmod(base.RuntimeHome, 0700); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"add", "remove", "base-url", "hosts"} {
		t.Run(change, func(t *testing.T) {
			before, after := base, base
			sb := *base.Sandbox
			sb.Edge = &Edge{}
			if change == "add" {
				after.Sandbox = &sb
			} else {
				before.Sandbox = &sb
				changed := sb
				edge := *sb.Edge
				changed.Edge = &edge
				after.Sandbox = &changed
				switch change {
				case "remove":
					after.Sandbox = base.Sandbox
				case "base-url":
					edge.BaseURL = "https://custom.example/v1"
				case "hosts":
					edge.AllowHosts = []string{"custom.example:443"}
				}
			}
			normalized, err := normalize(before)
			if err != nil {
				t.Fatal(err)
			}
			ref := reference(normalized, "fixture-reference")
			if _, err = Resume(context.Background(), after, ref); !errors.Is(err, ErrIncompatibleResume) {
				t.Fatal("mismatch launched", err)
			}
			if calls != 0 {
				t.Fatal("mismatch probed")
			}
		})
	}
}
