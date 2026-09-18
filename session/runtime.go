package session

import (
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

// A restricted session needs two things that pull in opposite directions: a
// configuration nobody else can add to, and the operator's existing login.
//
// Taking the operator's home gives the login and everything else in it —
// servers, hooks, plugins, project trust — and no flag on the installed Codex
// build removes them: `--ignore-user-config` does not exist on `app-server`,
// and overriding the table with `-c mcp_servers={}` was observed not to clear
// entries already declared there. Refusing any home that contains such settings
// would reject nearly every real one, which is not a boundary, it is a demand
// that people dismantle their own tools.
//
// So a restricted session gets its own durable runtime home, with configuration
// this library writes, and shares exactly one thing from the source home: the
// credential. It moves between two owner-only directories in private
// application state, never reaching a workspace, a tool result, a model, an
// error or a log, and no API key is substituted for it.
//
// Sharing a login means two writers, so which copy is authoritative has to be
// decided rather than assumed. The runtime home records the digest of the
// credential it was given. A source that still matches that record has not
// changed, so the runtime copy — which the harness may have refreshed — wins. A
// source that no longer matches has been changed by the operator or another
// worker, so it wins and the runtime copy is replaced. Neither side is ever
// overwritten on the strength of being newer to arrive.

const (
	codexConfigFile     = "config.toml"
	codexCredentialFile = "auth.json"
	// sharedLoginRecord remembers which source credential this runtime home was
	// given, so a later refresh can be told apart from a changed account.
	sharedLoginRecord = "login.shared"
)

// runtimeConfig is the whole configuration a restricted session runs with. It
// is deliberately tiny: everything that matters is passed as explicit overrides
// at launch, and what is here exists so the home is well-formed rather than
// inherited.
const runtimeConfig = "# Written by lib-agent-harness for a restricted session.\n" +
	"# Configuration for these sessions is supplied at launch; edits here are\n" +
	"# replaced, and inherited settings are deliberately not read.\n"

type sharedLogin struct {
	// Source is the digest of the credential this home was given.
	Source string `json:"source"`
	// SharedAt is when it was taken. It is diagnostic; decisions use digests,
	// because a clock says nothing about which copy is the account.
	SharedAt time.Time `json:"shared_at"`
}

// prepareRuntimeHome makes the private home a restricted session runs in and
// shares the source login into it. It returns the home to launch with.
func prepareRuntimeHome(source, runtime string) (string, error) {
	if runtime == "" {
		return "", errors.New("a restricted session requires a durable runtime home; set Options.RuntimeHome")
	}
	// Decided before anything is written. A runtime home's configuration is
	// this library's to replace, and replacing an operator's own home with it
	// would destroy exactly the settings this design exists to leave alone.
	same, err := sameDirectory(source, runtime)
	if err != nil {
		return "", err
	}
	if same {
		return "", errors.New("a restricted session's runtime home must be separate from the login source home")
	}
	if err = os.MkdirAll(runtime, 0700); err != nil {
		return "", errors.New("restricted session runtime home could not be created")
	}
	if err = os.Chmod(runtime, 0700); err != nil {
		return "", errors.New("restricted session runtime home could not be restricted")
	}
	info, err := os.Lstat(runtime)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("restricted session runtime home is not a usable directory")
	}
	if err = ownerOnly(info); err != nil {
		return "", err
	}
	// Rewrite the configuration every launch. A runtime home is this library's,
	// and a session must not inherit an edit made to it between runs.
	if err = writePrivate(filepath.Join(runtime, codexConfigFile), []byte(runtimeConfig)); err != nil {
		return "", errors.New("restricted session runtime configuration could not be written")
	}
	if err = shareCredential(source, runtime); err != nil {
		return "", err
	}
	return runtime, nil
}

// sameDirectory reports whether two paths name the same directory.
//
// Comparing text is not enough. A case-insensitive filesystem answers to
// several spellings of one path, a symlink gives it another name, and a hard
// link or bind mount gives it one with no textual relationship at all. So where
// both paths exist the question is put to the filesystem, which is the only
// thing that actually knows. Text comparison remains as a fallback for a path
// that does not exist yet, where there is nothing to ask about.
func sameDirectory(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	left, err := filepath.Abs(a)
	if err != nil {
		return false, errors.New("harness home path could not be resolved")
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return false, errors.New("harness home path could not be resolved")
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

// shareCredential gives the runtime home a login to use, deciding which copy is
// the account rather than assuming the source always is.
func shareCredential(source, runtime string) error {
	from := filepath.Join(source, codexCredentialFile)
	to := filepath.Join(runtime, codexCredentialFile)
	present, err := credentialDigest(from)
	if err != nil {
		return err
	}
	if present == nil {
		// No file-backed login in the source home. Some builds keep their
		// credential elsewhere, and this library will not go looking: say which
		// home was checked and let the operator log in there.
		return &CapabilityError{Engine: string(Codex), Code: CapabilityLoginUnavailable, Phase: BeforeLaunch}
	}
	existing, err := credentialDigest(to)
	if err != nil {
		return err
	}
	shared := readSharedLogin(runtime)
	switch {
	case existing == nil:
		// Nothing here yet.
	case shared != nil && shared.Source == hex.EncodeToString(present):
		// The source is exactly what this home was given, so it has not changed.
		// Whatever is here now is either that same credential or a refresh the
		// harness performed; either way it is at least as current, and replacing
		// it would undo a refresh that a crash prevented writing back.
		return nil
	case subtle.ConstantTimeCompare(existing, present) == 1:
		// Identical without a record — record it and move on.
		return recordSharedLogin(runtime, present)
	}
	// Either this home has no login, or the source has changed since it was
	// given one. A changed source is the operator logging in again, and that
	// wins.
	if err = copyPrivate(from, to); err != nil {
		return err
	}
	return recordSharedLogin(runtime, present)
}

// writeBackCredential returns a refreshed login to the source home, so the next
// worker and the operator's own CLI keep one account rather than drifting into
// a private copy that quietly expires.
//
// It declines when the source has changed since this home was given its
// credential: that is a newer login, and a worker holding an older one must not
// reinstate it.
func writeBackCredential(source, runtime string) error {
	from := filepath.Join(runtime, codexCredentialFile)
	to := filepath.Join(source, codexCredentialFile)
	refreshed, err := credentialDigest(from)
	if err != nil || refreshed == nil {
		return err
	}
	shared := readSharedLogin(runtime)
	if shared == nil {
		// Nothing establishes what this home started from, so nothing
		// establishes that its copy is newer. Leave the source alone.
		return nil
	}
	if shared.Source == hex.EncodeToString(refreshed) {
		return nil // unchanged; there is nothing to return
	}
	current, err := credentialDigest(to)
	if err != nil {
		return err
	}
	// Write-back updates a login that is still the one this worker was given.
	// An absent source is a logout — someone deliberately removed that account —
	// and recreating it from a worker's copy would undo it. A changed source is
	// a newer login, and reinstating an older one over it is the same mistake in
	// the other direction. Neither is a write-back; both leave the source alone.
	if current == nil || hex.EncodeToString(current) != shared.Source {
		return nil
	}
	if err = copyPrivate(from, to); err != nil {
		return err
	}
	return recordSharedLogin(runtime, refreshed)
}

func readSharedLogin(runtime string) *sharedLogin {
	raw, err := os.ReadFile(filepath.Join(runtime, sharedLoginRecord))
	if err != nil {
		return nil
	}
	var record sharedLogin
	if json.Unmarshal(raw, &record) != nil || record.Source == "" {
		return nil
	}
	return &record
}

func recordSharedLogin(runtime string, digest []byte) error {
	raw, err := json.Marshal(sharedLogin{Source: hex.EncodeToString(digest), SharedAt: time.Now().UTC()})
	if err != nil {
		return errors.New("shared login record could not be encoded")
	}
	return writePrivate(filepath.Join(runtime, sharedLoginRecord), raw)
}

// credentialDigest identifies a credential file without revealing it. A missing
// file is nil; anything else that cannot be read is an error, because treating
// an unreadable login as absent would replace a working one.
func credentialDigest(path string) ([]byte, error) {
	file, err := openNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("harness login could not be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("harness login is not a readable credential file")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, 1<<20)); err != nil {
		return nil, errors.New("harness login could not be read")
	}
	return hash.Sum(nil), nil
}

// copyPrivate writes a credential to an owner-only file and renames it into
// place, so a reader never sees a half-written login and a crash never leaves
// one truncated. The bytes are never held anywhere else.
func copyPrivate(from, to string) error {
	source, err := openNoFollow(from)
	if err != nil {
		return errors.New("harness login could not be read")
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(to), ".login-")
	if err != nil {
		return errors.New("harness login could not be shared")
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return errors.New("harness login could not be restricted")
	}
	if _, err = io.Copy(temporary, io.LimitReader(source, 1<<20)); err != nil {
		_ = temporary.Close()
		return errors.New("harness login could not be shared")
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("harness login could not be shared")
	}
	if err = temporary.Close(); err != nil {
		return errors.New("harness login could not be shared")
	}
	if err = os.Rename(name, to); err != nil {
		return errors.New("harness login could not be shared")
	}
	return nil
}
