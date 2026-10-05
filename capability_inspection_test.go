package harness

import (
	"runtime"
	"testing"
)

func TestProcessInspectionSupport(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows", "freebsd"} {
		for _, e := range []Engine{Codex, Claude, Grok, CommandCode, OpenAICompatible, Engine("invalid")} {
			for _, op := range Operations() {
				c := processInspectionSupport(e, op, goos)
				want := Unsupported
				if goos == "linux" && e == OpenAICompatible && op == Session {
					want = Unknown
				}
				if c.Availability != want || c.Reason == "" {
					t.Errorf("%s %s %s: %+v", goos, e, op, c)
				}
				if goos == runtime.GOOS && Support(e, op, ProcessInspection) != c {
					t.Fatal("public capability differs from platform contract")
				}
			}
		}
	}
}
