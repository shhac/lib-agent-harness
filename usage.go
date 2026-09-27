package harness

// Usage is token accounting in one shape for every engine and mode. Known
// distinguishes unavailable accounting from zero usage: unknown is never free.
//
// Input counts every prompt token, cached or not, which is the one figure every
// provider reports. The cache figures are parts of Input, and CacheKnown says
// the provider actually split them out: many gateways omit the split, and zero
// cache figures then mean unreported, not uncached.
type Usage struct {
	Known      bool  `json:"known"`
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`      // includes Reasoning
	CacheRead  int64 `json:"cache_read"`  // part of Input
	CacheWrite int64 `json:"cache_write"` // part of Input
	Reasoning  int64 `json:"reasoning"`   // part of Output; never added to a total
	CacheKnown bool  `json:"cache_known"`
}

// Fresh is the input neither read from nor written to the cache. It is
// derived rather than stored because it cannot always be derived.
func (u Usage) Fresh() (int64, bool) {
	if !u.Known || !u.CacheKnown {
		return 0, false
	}
	return u.Input - u.CacheRead - u.CacheWrite, true
}

func (u Usage) Total() int64 { return u.Input + u.Output }

// Cost is token spend valued in US dollars at the provider's API rates, as the
// harness reports it. It is a valuation, not a bill: on a subscription the
// same tokens draw on quota windows instead, and credits may cover either.
// Known false means partial or unreported, never zero.
type Cost struct {
	USD   float64 `json:"usd"`
	Known bool    `json:"known"`
}
