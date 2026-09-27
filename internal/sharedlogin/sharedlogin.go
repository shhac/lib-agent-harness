// Package sharedlogin gives a library-owned runtime home an operator's native
// login without inheriting anything else from the operator's home.
//
// Taking the operator's home would bring its servers, hooks, plugins and rules
// with the login. So the runtime home holds configuration this library writes,
// and shares exactly one file from the source home: the credential. It moves
// between two owner-only directories and never reaches a workspace, a model, an
// error or a log.
//
// Sharing a login means two writers, so which copy is authoritative is decided
// rather than assumed. The runtime home records the digest of the credential it
// was given. A source that still matches the record has not changed, so the
// runtime copy, which the harness may have refreshed, wins. A source that no
// longer matches was changed by the operator or another worker, so it wins.
// Neither side is ever overwritten on the strength of being newer to arrive.
package sharedlogin

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Record remembers which source credential a runtime home was given, so a
// later refresh can be told apart from a changed account.
const Record = "login.shared"

const lockFile = "login.lock"

// maxCredential bounds a credential file; anything larger is not one.
const maxCredential = 1 << 20

// Error carries a fixed code and never a path, credential or OS message.
type Error struct{ Code string }

func (e *Error) Error() string { return "shared login: " + e.Code }

// Codes a caller maps into its own vocabulary.
const (
	CodeRuntimeRequired  = "runtime_home_required"
	CodeRuntimeIsSource  = "runtime_home_is_source"
	CodeRuntimeUnusable  = "runtime_home_unusable"
	CodeConfigWrite      = "runtime_config_write"
	CodeLoginUnavailable = "login_unavailable"
	CodeLoginUnreadable  = "login_unreadable"
	CodeLoginShare       = "login_share_failed"
	CodeUnsupported      = "platform_unsupported"
)

func failure(code string) error { return &Error{Code: code} }

// Home is one runtime home and the source home whose login it shares.
type Home struct {
	Source  string
	Runtime string
	// Credential is the login's file name, the same in both homes.
	Credential string
	// Files are configuration this library owns, rewritten on every Prepare so
	// nothing edited between runs is inherited.
	Files map[string][]byte
	// Valid, when set, must accept a credential before it is copied either
	// way, so a file caught halfway through a rewrite is never shared.
	Valid func([]byte) bool
}

// Prepare makes the runtime home, writes its configuration and shares the
// source login into it.
func (h Home) Prepare() error {
	if !supported() {
		return failure(CodeUnsupported)
	}
	if h.Runtime == "" || h.Credential == "" {
		return failure(CodeRuntimeRequired)
	}
	// Decided before anything is written: this library replaces a runtime
	// home's configuration, and doing that to the operator's own home would
	// destroy exactly the settings this design leaves alone.
	same, err := sameDirectory(h.Source, h.Runtime)
	if err != nil {
		return err
	}
	if same {
		return failure(CodeRuntimeIsSource)
	}
	if err = os.MkdirAll(h.Runtime, 0700); err != nil {
		return failure(CodeRuntimeUnusable)
	}
	if err = os.Chmod(h.Runtime, 0700); err != nil {
		return failure(CodeRuntimeUnusable)
	}
	info, err := os.Lstat(h.Runtime)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return failure(CodeRuntimeUnusable)
	}
	if !ownerOnly(info) {
		return failure(CodeRuntimeUnusable)
	}
	unlock, err := lock(filepath.Join(h.Runtime, lockFile))
	if err != nil {
		return failure(CodeRuntimeUnusable)
	}
	defer unlock()
	for name, data := range h.Files {
		if filepath.Base(name) != name || name == h.Credential || name == Record || name == lockFile {
			return failure(CodeConfigWrite)
		}
		if err = writePrivate(filepath.Join(h.Runtime, name), data); err != nil {
			return failure(CodeConfigWrite)
		}
	}
	return h.share()
}

func (h Home) share() error {
	from := filepath.Join(h.Source, h.Credential)
	to := filepath.Join(h.Runtime, h.Credential)
	present, err := h.read(from)
	if err != nil {
		return err
	}
	if present == nil {
		// No file-backed login in the source home. This library will not go
		// looking elsewhere: the operator logs in to that home.
		return failure(CodeLoginUnavailable)
	}
	presentDigest := digest(present)
	existing, err := readCredential(to)
	if err != nil {
		return err
	}
	shared := readRecord(h.Runtime)
	switch {
	case existing == nil:
	case shared != nil && shared.Source == hex.EncodeToString(presentDigest):
		// The source is exactly what this home was given, so whatever is here
		// is that credential or a refresh the harness made. Replacing it would
		// undo a refresh a crash kept from being written back.
		return nil
	case subtle.ConstantTimeCompare(digest(existing), presentDigest) == 1:
		return writeRecord(h.Runtime, presentDigest)
	}
	// No login here yet, or the source changed since this home was given one:
	// the operator logged in again, and that wins.
	if err = writePrivate(to, present); err != nil {
		return failure(CodeLoginShare)
	}
	return writeRecord(h.Runtime, presentDigest)
}

// WriteBack returns a refreshed login to the source home, so the operator's
// own CLI and the next worker keep one account rather than drifting into a
// private copy that quietly expires. It declines when the source changed or
// was removed since this home was given its credential: a newer login or a
// logout wins over a worker's copy.
func (h Home) WriteBack() error {
	if !supported() {
		return failure(CodeUnsupported)
	}
	if _, err := os.Lstat(h.Runtime); errors.Is(err, os.ErrNotExist) {
		// A runtime home that was never made holds no refresh to return.
		return nil
	}
	unlock, err := lock(filepath.Join(h.Runtime, lockFile))
	if err != nil {
		return failure(CodeRuntimeUnusable)
	}
	defer unlock()
	refreshed, err := h.read(filepath.Join(h.Runtime, h.Credential))
	if err != nil || refreshed == nil {
		return err
	}
	shared := readRecord(h.Runtime)
	if shared == nil {
		// Nothing establishes what this home started from, so nothing
		// establishes that its copy is newer.
		return nil
	}
	refreshedDigest := digest(refreshed)
	if shared.Source == hex.EncodeToString(refreshedDigest) {
		return nil
	}
	current, err := readCredential(filepath.Join(h.Source, h.Credential))
	if err != nil {
		return err
	}
	if current == nil || hex.EncodeToString(digest(current)) != shared.Source {
		return nil
	}
	if err = writePrivate(filepath.Join(h.Source, h.Credential), refreshed); err != nil {
		return failure(CodeLoginShare)
	}
	return writeRecord(h.Runtime, refreshedDigest)
}

// Digest identifies the credential at path without revealing it. A missing
// file is nil; anything else that cannot be read, including a symbolic link,
// is an error.
func Digest(path string) ([]byte, error) {
	if !supported() {
		return nil, failure(CodeUnsupported)
	}
	data, err := readCredential(path)
	if err != nil || data == nil {
		return nil, err
	}
	return digest(data), nil
}

// read is readCredential with the caller's validity check: an invalid file
// is treated as unreadable rather than absent, so it replaces nothing.
func (h Home) read(path string) ([]byte, error) {
	data, err := readCredential(path)
	if err != nil || data == nil {
		return data, err
	}
	if h.Valid != nil && !h.Valid(data) {
		return nil, failure(CodeLoginUnreadable)
	}
	return data, nil
}

type record struct {
	// Source is the digest of the credential this home was given.
	Source string `json:"source"`
	// SharedAt is diagnostic; decisions use digests, because a clock says
	// nothing about which copy is the account.
	SharedAt time.Time `json:"shared_at"`
}

func readRecord(runtime string) *record {
	raw, err := os.ReadFile(filepath.Join(runtime, Record))
	if err != nil {
		return nil
	}
	var r record
	if json.Unmarshal(raw, &r) != nil || r.Source == "" {
		return nil
	}
	return &r
}

func writeRecord(runtime string, sum []byte) error {
	raw, err := json.Marshal(record{Source: hex.EncodeToString(sum), SharedAt: time.Now().UTC()})
	if err != nil {
		return failure(CodeLoginShare)
	}
	if err = writePrivate(filepath.Join(runtime, Record), raw); err != nil {
		return failure(CodeLoginShare)
	}
	return nil
}

func digest(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// readCredential reads a credential without following a final symbolic link.
// A missing file is nil; anything else unreadable is an error, because
// treating an unreadable login as absent would replace a working one.
func readCredential(path string) ([]byte, error) {
	file, err := openNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, failure(CodeLoginUnreadable)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCredential {
		return nil, failure(CodeLoginUnreadable)
	}
	var buffer bytes.Buffer
	if _, err = io.Copy(&buffer, io.LimitReader(file, maxCredential+1)); err != nil || buffer.Len() > maxCredential {
		return nil, failure(CodeLoginUnreadable)
	}
	return buffer.Bytes(), nil
}

// writePrivate replaces path with an owner-only file holding data, written
// beside it and renamed into place: a reader sees the old content or the new,
// never a truncated file, and a link planted at path is replaced rather than
// written through.
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := f.Name()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// sameDirectory reports whether two paths name the same directory. Text is
// not enough: case-insensitive filesystems, symlinks, hard links and bind
// mounts all give one directory several names, so where both exist the
// filesystem is asked.
func sameDirectory(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	left, err := filepath.Abs(a)
	if err != nil {
		return false, failure(CodeRuntimeUnusable)
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return false, failure(CodeRuntimeUnusable)
	}
	if left == right {
		return true, nil
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo), nil
	}
	if resolved, err := filepath.EvalSymlinks(left); err == nil {
		left = resolved
	}
	if resolved, err := filepath.EvalSymlinks(right); err == nil {
		right = resolved
	}
	return left == right, nil
}
