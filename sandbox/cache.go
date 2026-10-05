package sandbox

import (
	"context"
	"sync"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

// verified remembers capability checks for this process only. Nothing is
// written to disk: a restart re-proves, because a restart is exactly when an
// installed CLI is most likely to have changed underneath.
var verified = &verificationCache{seen: map[string]bool{}}

type verificationCache struct {
	mu       sync.Mutex
	seen     map[string]bool
	networks map[string]networkEvidence
	pending  map[string]chan struct{}
}

// Serialize only identical proofs; unrelated keys and settled hits never queue
// behind another runtime's network witness.
func (c *verificationCache) acquire(ctx context.Context, key string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.pending == nil {
			c.pending = map[string]chan struct{}{}
		}
		if wait, ok := c.pending[key]; ok {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wait := make(chan struct{})
		c.pending[key] = wait
		c.mu.Unlock()
		return func() { c.mu.Lock(); delete(c.pending, key); close(wait); c.mu.Unlock() }, nil
	}
}

func (c *verificationCache) holds(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}
func (c *verificationCache) settledNetwork(key string) (networkEvidence, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.networks[key]
	e.Observations = append([]InterfaceObservation(nil), e.Observations...)
	return e, c.seen[key]
}
func (c *verificationCache) record(key string) { c.recordWithNetwork(key, networkEvidence{}) }
func (c *verificationCache) recordWithNetwork(key string, evidence networkEvidence) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > 64 {
		c.seen = map[string]bool{}
		c.networks = nil
	}
	if c.networks == nil {
		c.networks = map[string]networkEvidence{}
	}
	evidence.Observations = append([]InterfaceObservation(nil), evidence.Observations...)
	c.networks[key] = evidence
	c.seen[key] = true
}

type selectedPortEvidence struct {
	control      sandboxprobe.ControlTarget
	observations []InterfaceObservation
}

type networkEvidence struct {
	control      sandboxprobe.ControlTarget
	Observations []InterfaceObservation
	Detail       string
}

func (c *verificationCache) network(key string) networkEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.networks[key]
	e.Observations = append([]InterfaceObservation(nil), e.Observations...)
	return e
}
