package session

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// aliasedNames: a Windows volume may give a file an 8.3 short name beside
// its own, so a name the model supplied does not say what it opens.
const aliasedNames = true

// realName is the last element of the opened file's final path, which
// Windows gives with its long names, however the file was reached.
func realName(f *os.File) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return filepath.Base(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1)
	}
}
