//go:build !windows

package sandbox

import (
	"errors"
	"os"
)

// aliasedNames: outside Windows a name reaches a file only as spelled, up to
// case on a case-insensitive volume, which the policies compare without.
const aliasedNames = false

// realName is never called where aliasedNames is false.
func realName(*os.File) (string, error) {
	return "", errors.New("real names are not read on this platform")
}
