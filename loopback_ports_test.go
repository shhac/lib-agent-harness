package harness

import (
	"runtime"
	"testing"
)

func TestLoopbackPortsSupport(t *testing.T) {
	for _, engine := range []Engine{Codex, Claude, Grok, CommandCode, OpenAICompatible} {
		for _, op := range []Operation{Session, Run} {
			want := Unsupported
			if engine == OpenAICompatible && op == Session && runtime.GOOS == "darwin" {
				want = Unknown
			}
			got := Support(engine, op, LoopbackPorts)
			if got.Availability != want || got.Reason == "" {
				t.Fatalf("%s %s: %+v", engine, op, got)
			}
		}
	}
}
