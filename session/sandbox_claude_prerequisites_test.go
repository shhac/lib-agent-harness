package session

import (
	"errors"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestClaudeSocatPrerequisiteIsPlatformAndModeScoped(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, engine := range []harness.Engine{harness.Claude, harness.Codex} {
			for _, sandboxed := range []bool{false, true} {
				o := Options{Provider: harness.Provider{Engine: engine}}
				if sandboxed {
					o.Sandbox = &Sandbox{}
				}
				called := false
				err := checkClaudeSandboxPrerequisites(goos, o, func(name string) (string, error) {
					called = true
					if name != "socat" {
						t.Fatal(name)
					}
					return "", errors.New("secret host diagnostic")
				})
				want := goos == "linux" && engine == harness.Claude && sandboxed
				if called != want || (err != nil) != want {
					t.Fatal(goos, engine, sandboxed, err)
				}
				if want {
					e, ok := err.(*CapabilityError)
					if !ok || e.Code != CapabilitySandboxUnavailable || e.Reason != ClaudeLinuxSandboxRequiresSocat || len(e.Tools) != 0 || !strings.Contains(e.Error(), "install") || strings.Contains(e.Error(), "secret") {
						t.Fatal(err)
					}
				}
			}
		}
	}
	o := Options{Provider: harness.Provider{Engine: harness.Claude}, Sandbox: &Sandbox{}}
	if err := checkClaudeSandboxPrerequisites("linux", o, func(string) (string, error) { return "/proved/socat", nil }); err != nil {
		t.Fatal(err)
	}
}
