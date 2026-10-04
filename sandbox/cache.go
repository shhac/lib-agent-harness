package sandbox

import "sync"

// verified remembers capability checks for this process only. Nothing is
// written to disk: a restart re-proves, because a restart is exactly when an
// installed CLI is most likely to have changed underneath.
var verified = &verificationCache{seen: map[string]bool{}}

type verificationCache struct {
	mu       sync.Mutex
	seen     map[string]bool
	controls map[string]networkControl
}

// Selected-port evidence and its destination facts are published atomically.
func (c *verificationCache) control(key string) (networkControl, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.controls[key]
	return v, ok && c.seen[key]
}
func (c *verificationCache) recordControl(key string, v networkControl) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > 64 {
		c.seen = map[string]bool{}
		c.controls = nil
	}
	if c.controls == nil {
		c.controls = map[string]networkControl{}
	}
	c.controls[key] = v
	c.seen[key] = true
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
		c.controls = nil
	}
	c.seen[key] = true
}
