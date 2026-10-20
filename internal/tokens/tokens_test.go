package tokens

import (
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func ptr(v int64) *int64 { return &v }

const huge = int64(^uint64(0)>>1) - 1

func TestCacheInclusive(t *testing.T) {
	for name, c := range map[string]struct {
		r    Report
		want harness.Usage
	}{
		"split stated": {Report{Input: ptr(100), Output: ptr(10), CacheRead: ptr(60), CacheWrite: ptr(5), Reasoning: ptr(3)},
			harness.Usage{Known: true, Input: 100, Output: 10, CacheRead: 60, CacheWrite: 5, Reasoning: 3, CacheKnown: true, ReasoningKnown: true}},
		"one cache figure is not a split": {Report{Input: ptr(100), Output: ptr(10), CacheRead: ptr(60)},
			harness.Usage{Known: true, Input: 100, Output: 10, CacheRead: 60}},
		"no input":                {Report{Output: ptr(1)}, harness.Usage{}},
		"no output":               {Report{Input: ptr(1)}, harness.Usage{}},
		"negative input":          {Report{Input: ptr(-1), Output: ptr(1)}, harness.Usage{}},
		"negative output":         {Report{Input: ptr(1), Output: ptr(-1)}, harness.Usage{}},
		"negative cache":          {Report{Input: ptr(5), Output: ptr(1), CacheRead: ptr(-1), CacheWrite: ptr(0)}, harness.Usage{}},
		"cache beyond input":      {Report{Input: ptr(5), Output: ptr(1), CacheRead: ptr(4), CacheWrite: ptr(2)}, harness.Usage{}},
		"reasoning beyond output": {Report{Input: ptr(5), Output: ptr(1), Reasoning: ptr(2)}, harness.Usage{}},
		"total overflows":         {Report{Input: ptr(huge), Output: ptr(huge)}, harness.Usage{}},
	} {
		if got := CacheInclusive(c.r); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}

func TestCacheExclusive(t *testing.T) {
	for name, c := range map[string]struct {
		r    Report
		want harness.Usage
	}{
		"cache added back": {Report{Input: ptr(10), Output: ptr(4), CacheRead: ptr(100), CacheWrite: ptr(20)},
			harness.Usage{Known: true, Input: 130, Output: 4, CacheRead: 100, CacheWrite: 20, CacheKnown: true}},
		"absent write leaves the split unknown": {Report{Input: ptr(10), Output: ptr(4), CacheRead: ptr(100)},
			harness.Usage{Known: true, Input: 110, Output: 4, CacheRead: 100}},
		"negative cache":     {Report{Input: ptr(10), Output: ptr(4), CacheRead: ptr(-1), CacheWrite: ptr(0)}, harness.Usage{}},
		"input overflows":    {Report{Input: ptr(huge), Output: ptr(1), CacheRead: ptr(huge), CacheWrite: ptr(0)}, harness.Usage{}},
		"negative reasoning": {Report{Input: ptr(1), Output: ptr(1), Reasoning: ptr(-1)}, harness.Usage{}},
	} {
		if got := CacheExclusive(c.r); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}

func TestNoCacheWriteAndSum(t *testing.T) {
	if NoCacheWrite(nil) != nil {
		t.Fatal("an unstated read became a stated split")
	}
	if w := NoCacheWrite(ptr(3)); w == nil || *w != 0 {
		t.Fatalf("a stated read's write: %v", w)
	}
	if s := Sum(ptr(2), ptr(3)); s == nil || *s != 5 {
		t.Fatalf("sum: %v", s)
	}
	for name, parts := range map[string][]*int64{"absent": {ptr(1), nil}, "negative": {ptr(1), ptr(-2)}, "overflow": {ptr(huge), ptr(huge)}} {
		if Sum(parts...) != nil {
			t.Errorf("%s: a sum that is not a measurement was stated", name)
		}
	}
}

func TestAdd(t *testing.T) {
	split := harness.Usage{Known: true, Input: 10, Output: 2, CacheRead: 4, CacheKnown: true, Reasoning: 1, ReasoningKnown: true}
	bare := harness.Usage{Known: true, Input: 5, Output: 1}
	if got := Add(harness.Usage{}, split); got != split {
		t.Fatalf("an empty accumulation did not take the first addend: %+v", got)
	}
	got := Add(split, bare)
	if !got.Known || got.Input != 15 || got.Output != 3 || got.CacheRead != 4 || got.Reasoning != 1 || got.CacheKnown || got.ReasoningKnown {
		t.Fatalf("a split one addend omitted stayed known: %+v", got)
	}
	if got := Add(bare, split); got.CacheKnown || got.ReasoningKnown {
		t.Fatalf("a later split made an earlier omission known: %+v", got)
	}
	if got := Add(split, split); !got.CacheKnown || !got.ReasoningKnown || got.Input != 20 {
		t.Fatalf("a split every addend stated was lost: %+v", got)
	}
	if got := Add(split, harness.Usage{}); got.Known {
		t.Fatalf("an unknown addend was read as free: %+v", got)
	}
	if got := Add(harness.Usage{Known: true, Input: huge}, harness.Usage{Known: true, Input: huge}); got.Known {
		t.Fatalf("an overflowing sum stayed known: %+v", got)
	}
	if got := Add(split, harness.Usage{Known: true, Input: -1}); got.Known {
		t.Fatalf("a negative addend stayed known: %+v", got)
	}
}
