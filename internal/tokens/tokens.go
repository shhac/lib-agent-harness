// Package tokens turns a provider's token report into harness.Usage, and
// accumulates usage, with one validation for every engine and mode. Providers
// report in one of two shapes: input that already counts the cache figures,
// and input that excludes them. Either way a negative figure, a part larger
// than its whole, or a sum that would not stay representable makes the usage
// unknown rather than a number that looks measured, and a cache or reasoning
// split is known only when the provider actually stated it.
package tokens

import harness "github.com/shhac/lib-agent-harness"

// Report is a provider's figures, with an absent one nil.
type Report struct {
	Input, Output         *int64
	CacheRead, CacheWrite *int64
	// Reasoning is the part of Output that was reasoning.
	Reasoning *int64
}

// CacheInclusive reads a report whose Input already counts the cache reads
// and writes.
func CacheInclusive(r Report) harness.Usage {
	if r.Input == nil || r.Output == nil {
		return harness.Usage{}
	}
	read, write := value(r.CacheRead), value(r.CacheWrite)
	if read < 0 || write < 0 || read > *r.Input || write > *r.Input-read {
		return harness.Usage{}
	}
	return finish(r, *r.Input)
}

// CacheExclusive reads a report whose Input excludes both cache figures, so
// every prompt token is the sum of all three.
func CacheExclusive(r Report) harness.Usage {
	if r.Input == nil || r.Output == nil {
		return harness.Usage{}
	}
	input, ok := sum(*r.Input, value(r.CacheRead), value(r.CacheWrite))
	if !ok {
		return harness.Usage{}
	}
	return finish(r, input)
}

func finish(r Report, input int64) harness.Usage {
	output, reasoning := *r.Output, value(r.Reasoning)
	if reasoning < 0 || reasoning > output {
		return harness.Usage{}
	}
	if _, ok := sum(input, output); !ok {
		return harness.Usage{}
	}
	return harness.Usage{
		Known:          true,
		Input:          input,
		Output:         output,
		CacheRead:      value(r.CacheRead),
		CacheWrite:     value(r.CacheWrite),
		Reasoning:      reasoning,
		CacheKnown:     r.CacheRead != nil && r.CacheWrite != nil,
		ReasoningKnown: r.Reasoning != nil,
	}
}

// Add accumulates next into acc. An unknown acc holds nothing yet, so the
// result is next; an unknown next, or a sum that would not stay
// representable, makes the result unknown. A split stays known only while
// every addend stated it.
func Add(acc, next harness.Usage) harness.Usage {
	if !next.Known {
		return harness.Usage{}
	}
	if !acc.Known {
		return next
	}
	out := harness.Usage{Known: true, CacheKnown: acc.CacheKnown && next.CacheKnown, ReasoningKnown: acc.ReasoningKnown && next.ReasoningKnown}
	for _, c := range []struct {
		into     *int64
		acc, add int64
	}{
		{&out.Input, acc.Input, next.Input},
		{&out.Output, acc.Output, next.Output},
		{&out.CacheRead, acc.CacheRead, next.CacheRead},
		{&out.CacheWrite, acc.CacheWrite, next.CacheWrite},
		{&out.Reasoning, acc.Reasoning, next.Reasoning},
	} {
		total, ok := sum(c.acc, c.add)
		if !ok {
			return harness.Usage{}
		}
		*c.into = total
	}
	return out
}

// NoCacheWrite is the cache write of an engine whose caching is implicit and
// never writes: once the provider states its cache read, that read is the
// whole split. An unstated read leaves the split unknown.
func NoCacheWrite(read *int64) *int64 {
	if read == nil {
		return nil
	}
	return new(int64)
}

// Sum is the total of figures a provider states apart, such as output and the
// reasoning it reports beside it, or nil when a figure is absent or negative
// or the total would not stay representable.
func Sum(parts ...*int64) *int64 {
	values := make([]int64, 0, len(parts))
	for _, part := range parts {
		if part == nil {
			return nil
		}
		values = append(values, *part)
	}
	total, ok := sum(values...)
	if !ok {
		return nil
	}
	return &total
}

func value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// sum adds non-negative figures, reporting false for a negative one or a
// total that would not stay representable.
func sum(parts ...int64) (int64, bool) {
	var total int64
	for _, part := range parts {
		if part < 0 || total > maxInt64-part {
			return 0, false
		}
		total += part
	}
	return total, true
}

const maxInt64 = int64(^uint64(0) >> 1)
