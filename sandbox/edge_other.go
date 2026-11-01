//go:build !darwin

package sandbox

import (
	"errors"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

func init() {
	sandboxbridge.BuildEdge = func(sandboxbridge.EdgeConfig) (sandboxbridge.EdgePlan, error) {
		return sandboxbridge.EdgePlan{}, errors.New("edge_unavailable")
	}
}
