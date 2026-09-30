package rawjson

import (
	"encoding/json"
	"testing"
)

func TestAbsent(t *testing.T) {
	for raw, want := range map[string]bool{"": true, "null": true, " null\n": true, "0": false, `""`: false, "{}": false, "[]": false, "false": false} {
		if Absent(json.RawMessage(raw)) != want {
			t.Errorf("%q", raw)
		}
	}
	if !Absent(nil) {
		t.Error("nil")
	}
}
