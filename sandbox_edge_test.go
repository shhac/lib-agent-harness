package harness

import (
	"runtime"
	"strings"
	"testing"
)

func TestSandboxEdgeSupportMatrix(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows", "other"} {
		for _, engine := range []Engine{Codex, Claude, Grok, CommandCode, OpenAICompatible, Engine("other")} {
			for _, op := range Operations() {
				got := sandboxEdgeSupport(engine, op, platform)
				if platform == runtime.GOOS && Support(engine, op, SandboxEdge) != got {
					t.Fatal("edge support dispatch lost in merge")
				}
				want := Unsupported
				if platform == "darwin" && op == Session && (engine == Codex || engine == Claude) {
					want = Unknown
					if !strings.Contains(got.Reason, "native TLS trust") || !strings.Contains(got.Reason, "unproved") || !strings.Contains(got.Reason, "fake-provider CA") {
						t.Fatal("production trust limit omitted", got)
					}
				}
				if got.Availability != want || got.Reason == "" {
					t.Fatalf("%s %s %s: %+v", platform, engine, op, got)
				}
				if platform == "linux" && op == Session && (engine == Codex || engine == Claude) && got.Reason != SandboxEdgeLinuxReason {
					t.Fatal("Linux reason changed")
				}
			}
		}
	}
	if SandboxEdge != "sandbox_edge" {
		t.Fatal("feature spelling changed")
	}
}
