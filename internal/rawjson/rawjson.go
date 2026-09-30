// Package rawjson answers the one question every protocol reader asks of a
// field it decoded raw: did the peer send a value at all. Absent and null are
// the same answer, so the check is spelled once.
package rawjson

import (
	"bytes"
	"encoding/json"
)

// Absent reports a field the peer left out or sent as null.
func Absent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
