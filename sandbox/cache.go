package sandbox

import "sync"

// verified remembers capability checks for this process only. Nothing is
// written to disk: a restart re-proves, because a restart is exactly when an
// installed CLI is most likely to have changed underneath.
var verified = &verificationCache{seen: map[string]bool{}}

type verificationCache struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (c *verificationCache) holds(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}
func (c *verificationCache) record(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > 64 {
		c.seen = map[string]bool{}
	}
	c.seen[key] = true
}
