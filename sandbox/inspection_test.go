package sandbox

import (
	"errors"
	"runtime"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestProcessInspectionEarlyRefusal(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	_, err := normalize(Options{ProcessInspection: true}, true)
	var refusal *RefusalError
	if !errors.As(err, &refusal) || refusal.Code != RefusedProcessInspectionUnenforceable {
		t.Fatalf("%v", err)
	}
	facts, _ := harness.ErrorFacts(err)
	if facts.Family != harness.FailureCapability {
		t.Fatalf("%+v", facts)
	}
}
