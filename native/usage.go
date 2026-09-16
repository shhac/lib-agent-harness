package native

type TokenUsage struct {
	Input      int // fresh input, never including cached reads
	Output     int
	CacheWrite int // context written to cache, which the model did process
	CacheRead  int // context re-read from cache, which it did not
	// Reasoning is part of Output, not an addition to it. Recorded because
	// it is worth analysing on its own; never summed into a total.
	Reasoning int
}

// Total is every token the run moved, cached re-reads included.
func (t TokenUsage) Total() int { return t.Input + t.Output + t.CacheWrite + t.CacheRead }

// Fresh is what the run actually processed: everything except context re-read
// from cache. The only token figure comparable across engines.
func (t TokenUsage) Fresh() int { return t.Input + t.Output + t.CacheWrite }
