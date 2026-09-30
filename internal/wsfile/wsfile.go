// Package wsfile holds the rules a model-supplied file path and a file's
// contents must meet before the library reads the file for a model: composed
// skills and the API workbench apply the same ones.
//
// A path is slash-separated and relative, and names the same file on every
// platform, so it may hold no NUL, control character, backslash or colon, and
// no element that Windows reinterprets as a device. Contents are text: valid
// UTF-8 with no NUL. How a file is reached, resolved or opened stays with the
// caller, because a skill directory and a workspace are contained differently.
package wsfile

import (
	"bytes"
	"io"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// PathProblem says why a relative path was refused.
type PathProblem int

const (
	// PathOK: the path is usable.
	PathOK PathProblem = iota
	// PathInvalid: the path is empty, too long, absolute, names a volume, or
	// holds a character no portable path may hold.
	PathInvalid
	// PathOutside: the path climbs out of its base when read lexically.
	PathOutside
)

// Clean accepts a slash-separated relative path that stays inside its base
// when read lexically, at most maxLength bytes long, and returns it cleaned.
// Backslashes and colons are refused outright, so one path means the same file
// on every platform.
func Clean(rel string, maxLength int) (string, PathProblem) {
	if rel == "" || len(rel) > maxLength || strings.ContainsAny(rel, "\x00\\:") || strings.HasPrefix(rel, "/") {
		return "", PathInvalid
	}
	if strings.IndexFunc(rel, Control) >= 0 {
		return "", PathInvalid
	}
	clean := path.Clean(rel)
	if clean == "." {
		return "", PathInvalid
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", PathOutside
	}
	native := filepath.FromSlash(clean)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", PathInvalid
	}
	return clean, PathOK
}

// Control reports a C0 or C1 control character, or DEL.
func Control(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// ReservedDevice reports whether any element of a cleaned slash path is a
// name Win32 reinterprets as a device: CON, PRN, AUX, NUL, COM0-COM9 or
// LPT0-LPT9 (superscript digits included), in any case, with any extension,
// and with trailing dots or spaces. It is refused on every platform, so a
// path means the same thing everywhere.
func ReservedDevice(clean string) bool {
	for _, element := range strings.Split(clean, "/") {
		if deviceName(element) {
			return true
		}
	}
	return false
}

func deviceName(element string) bool {
	stem := element
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) < 4 || (stem[:3] != "COM" && stem[:3] != "LPT") {
		return false
	}
	switch stem[3:] {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}

// Temporary names reserved for the workbench's own atomic writes:
// .harness-workbench-<session>-<random>.tmp.
const (
	reservedPrefix = ".harness-workbench-"
	reservedSuffix = ".tmp"
)

// Reserved reports whether a file name, the last element of a path, is in the
// workbench's reserved temporary pattern, whatever its session prefix. It
// compares without case and ignores trailing dots and spaces, so no spelling
// that a case-insensitive or Win32 volume folds onto such a name slips past.
func Reserved(name string) bool {
	name = strings.TrimRight(name, " .")
	if len(name) <= len(reservedPrefix)+len(reservedSuffix) {
		return false
	}
	return strings.EqualFold(name[:len(reservedPrefix)], reservedPrefix) && strings.EqualFold(name[len(name)-len(reservedSuffix):], reservedSuffix)
}

// Text reports whether data is text a model may be given: valid UTF-8 without
// a NUL.
func Text(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// ReadAtMost reads r to its end, or until it has read more than limit bytes.
// tooLarge reports the second case; data then holds limit+1 bytes. step, when
// set, is called before each chunk and stops the read with its error.
func ReadAtMost(r io.Reader, limit int64, step func() error) (data []byte, tooLarge bool, err error) {
	const chunk = 64 << 10
	var buf bytes.Buffer
	for {
		if step != nil {
			if err := step(); err != nil {
				return nil, false, err
			}
		}
		_, err := io.CopyN(&buf, r, min(chunk, limit+1-int64(buf.Len())))
		if int64(buf.Len()) > limit {
			return buf.Bytes(), true, nil
		}
		if err == io.EOF {
			return buf.Bytes(), false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}
