package harness

import "testing"

// Every engine is described, consistently with its transport, and nothing
// else is.
func TestEveryEngineIsDescribed(t *testing.T) {
	for _, e := range Engines() {
		d, ok := Describe(e)
		if !ok || d.Engine != e || d.Label == "" || d.Transport != e.Transport() {
			t.Errorf("%s: %+v", e, d)
		}
		cli := d.Transport == CLITransport
		if cli != (d.Binary != "") || cli != (d.HomeDir != "") {
			t.Errorf("%s: a CLI engine needs a binary and a home, an API engine neither: %+v", e, d)
		}
	}
	if _, ok := Describe("unknown"); ok {
		t.Error("an unknown engine was described")
	}
	if len(descriptors) != len(Engines()) {
		t.Error("a descriptor for an engine Engines does not list")
	}
}
