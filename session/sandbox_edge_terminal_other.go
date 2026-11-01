//go:build !darwin

package session

import "errors"

// foreignTerminal is darwin's alone: the edge is not offered elsewhere.
func foreignTerminal() (string, func(), error) {
	return "", nil, errors.New("edge_unavailable")
}
