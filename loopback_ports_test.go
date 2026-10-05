package harness

import (
	"runtime"
	"strings"
	"testing"
)

func TestLoopbackPortsSupport(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows", "other"} {
		for _, engine := range []Engine{Codex, Claude, Grok, CommandCode, OpenAICompatible, "unknown"} {
			for _, op := range []Operation{Session, Run, Complete, Models, Account, "unknown"} {
				got := loopbackPortsSupport(engine, op, goos)
				if got.Availability != Unsupported || !strings.Contains(got.Reason, "use Loopback") {
					t.Fatalf("%s/%s/%s: %+v", goos, engine, op, got)
				}
				if !strings.Contains(got.Reason, loopbackPortsAlternative) {
					t.Fatalf("%s/%s/%s: missing working command alternative: %s", goos, engine, op, got.Reason)
				}
				if goos == "darwin" && op == Session && !strings.Contains(got.Reason, LoopbackPortsSeatbeltReason) {
					t.Fatalf("%s: macOS session refusal omits Seatbelt reason: %s", engine, got.Reason)
				}
				if goos == runtime.GOOS && got != Support(engine, op, LoopbackPorts) {
					t.Fatal("public Support differs from platform matrix")
				}
				if engine == OpenAICompatible && op == Session {
					if goos == "darwin" && got.Reason != LoopbackPortsSeatbeltReason {
						t.Fatalf("Seatbelt reason: %s", got.Reason)
					}
					if goos == "linux" && got.Reason != loopbackPortsPrivateNamespaceReason {
						t.Fatalf("namespace reason: %s", got.Reason)
					}
				}
			}
		}
	}
}
