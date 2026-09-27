package skills

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Codes for reading or resolving a file inside a skill.
const (
	CodePathInvalid    = "file_path_invalid"
	CodeOutsideSkill   = "file_outside_skill"
	CodeFileNotFound   = "file_not_found"
	CodeFileNotRegular = "file_not_regular"
	CodeFileTooLarge   = "file_too_large"
	CodeFileNotText    = "file_not_text"
	CodeFileUnreadable = "file_unreadable"
)

// Read returns a UTF-8 text file inside the skill. rel is slash-separated and
// relative to the skill's directory.
//
// The file is resolved (every symbolic link followed) and checked before it
// is opened, and the opened file is checked to be the one resolved. A skill
// directory is the caller's own; one that is being rewritten concurrently can
// still race these checks, which contain the skill against a model's choice of
// path, not against its own author.
func (s Skill) Read(rel string) (string, error) {
	resolved, info, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	if info.Size() > MaxFileBytes {
		return "", fail(CodeFileTooLarge)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", openFailure(err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", fail(CodeFileUnreadable)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return "", fail(CodeFileUnreadable)
	}
	if len(data) > MaxFileBytes {
		return "", fail(CodeFileTooLarge)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", fail(CodeFileNotText)
	}
	return string(data), nil
}

// resolve finds a regular file inside the skill without opening it, so a
// named pipe or device is refused before anything could block on it.
func (s Skill) resolve(rel string) (string, fs.FileInfo, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(s.root, filepath.FromSlash(clean)))
	if err != nil {
		return "", nil, openFailure(err)
	}
	if !within(s.root, resolved) {
		return "", nil, fail(CodeOutsideSkill)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, openFailure(err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fail(CodeFileNotRegular)
	}
	return resolved, info, nil
}

// cleanRelative accepts a slash-separated relative path that stays inside
// its base when read lexically. Backslashes and colons are refused outright,
// so one path means the same file on every platform.
func cleanRelative(rel string) (string, error) {
	if rel == "" || len(rel) > maxRelativeLength || strings.ContainsAny(rel, "\x00\\:") || strings.HasPrefix(rel, "/") {
		return "", fail(CodePathInvalid)
	}
	if strings.IndexFunc(rel, control) >= 0 {
		return "", fail(CodePathInvalid)
	}
	clean := path.Clean(rel)
	if clean == "." {
		return "", fail(CodePathInvalid)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fail(CodeOutsideSkill)
	}
	native := filepath.FromSlash(clean)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", fail(CodePathInvalid)
	}
	return clean, nil
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func openFailure(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fail(CodeFileNotFound)
	}
	return fail(CodeFileUnreadable)
}
