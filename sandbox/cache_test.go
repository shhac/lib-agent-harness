package sandbox

import (
	"fmt"
	"sync"
	"testing"
)

func TestCommandVerificationCacheReset(t *testing.T) {
	c := &verificationCache{seen: map[string]bool{}}
	for i := range 65 {
		c.record(fmt.Sprint(i))
	}
	if len(c.seen) != 65 || !c.holds("0") {
		t.Fatal("reset boundary changed")
	}
	c.record("next")
	if len(c.seen) != 1 || c.holds("0") || !c.holds("next") {
		t.Fatal("cache did not reset after more than 64 keys")
	}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() { defer wg.Done(); key := fmt.Sprint(i); c.record(key); c.holds(key) }()
	}
	wg.Wait()
}
