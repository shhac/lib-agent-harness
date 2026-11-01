package session

import "errors"

// writeBeneath is unix's alone: the edge that needs it is not offered on
// Windows.
func writeBeneath(string, string, []byte) error {
	return errors.New("runtime writes beneath a root are unavailable on this platform")
}
