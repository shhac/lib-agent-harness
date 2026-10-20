//go:build darwin || linux

package sandbox

import (
	"testing"

	"github.com/shhac/lib-agent-harness/internal/skills"
)

// Each stream keeps a share of the result budget, capped at a skill script's
// output, and a budget too small for any share keeps nothing rather than a
// negative amount, on every platform.
func TestCommandOutputLimit(t *testing.T) {
	for _, c := range []struct {
		budget     int
		standalone bool
		want       int
	}{
		{1024 + 12*100, false, 100},
		{0, false, 0},
		{100, false, 0},
		{1 << 40, false, skills.MaxOutputBytes},
		{0, true, skills.MaxOutputBytes},
	} {
		if got := commandOutputLimit(c.budget, c.standalone); got != c.want {
			t.Errorf("%+v: %d", c, got)
		}
	}
}
