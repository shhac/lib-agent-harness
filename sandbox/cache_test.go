package sandbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestVerificationCacheEvidenceAndKeyAdmission(t *testing.T) {
	c := &verificationCache{seen: map[string]bool{}}
	release, err := c.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.acquire(ctx, "a"); err == nil {
		t.Fatal("same key admitted before settlement")
	}
	evidence := networkEvidence{Detail: "observed", Observations: []InterfaceObservation{{Errno: 13}}}
	c.recordWithNetwork("a", evidence)
	evidence.Observations[0].Errno = 0
	if !c.holds("a") || c.network("a").Observations[0].Errno != 13 {
		t.Fatal("cache lost or aliased evidence")
	}
	release()
	next, err := c.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	next()
}

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
