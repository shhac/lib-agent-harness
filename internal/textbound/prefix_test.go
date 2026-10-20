package textbound

import "testing"

func TestPrefixKeepsTheLimitAndCountsTheRest(t *testing.T) {
	p := &Prefix{Limit: 4}
	for _, chunk := range []string{"ab", "cdef", "gh"} {
		if n, err := p.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("a write was cut short: %d %v", n, err)
		}
	}
	if string(p.Bytes()) != "abcd" || !p.Exceeded() {
		t.Fatalf("%q exceeded=%v", p.Bytes(), p.Exceeded())
	}
	exact := &Prefix{Limit: 2}
	_, _ = exact.Write([]byte("ab"))
	if exact.Exceeded() {
		t.Fatal("output at the limit was reported as cut")
	}
}
