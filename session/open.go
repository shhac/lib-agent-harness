package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Opened says how Open produced a session.
type Opened struct {
	// Resumed is true when the reference's conversation was resumed.
	Resumed bool
	// Fresh is why a new conversation was started instead: "" when resumed or
	// when no reference was given, else FreshIncompatible or FreshUnavailable.
	Fresh string
}

const (
	FreshIncompatible = "incompatible" // the reference names another engine or configuration
	FreshUnavailable  = "unavailable"  // the conversation is gone or cannot be reopened
)

// errConversationGone marks a resume that failed because the harness no longer
// has the conversation, as opposed to one that failed for any other reason.
var errConversationGone = errors.New("harness conversation is unavailable to resume")

// Open resumes the reference's conversation when it can and otherwise starts a
// new one, saying which in Opened. It is the call for a caller that keeps one
// conversation across restarts and rebuilds it when it is lost.
//
// A restricted session's private directory is reclaimed first, under its
// assignment lease: a harness left running by a previous process is ended when
// it can be identified, and anything that cannot be confirmed gone is returned
// as ErrUnreclaimed — Open never starts another harness over an unconfirmed
// one. A lease held by another session is returned as ErrLeaseHeld.
//
// A nil reference starts. A reference for another engine or configuration
// starts with FreshIncompatible. A conversation the harness no longer has —
// Claude's transcript is missing, a resumed Claude exits during startup, or
// Codex rejects thread/resume — starts with FreshUnavailable. Every other
// failure is returned as is, with no session.
func Open(ctx context.Context, o Options, ref *Ref) (*Session, Opened, error) {
	n, err := normalize(o)
	if err != nil {
		return nil, Opened{}, err
	}
	if err = reclaimBeforeOpen(ctx, n); err != nil {
		return nil, Opened{}, err
	}
	if ref == nil {
		return openFresh(ctx, o, "")
	}
	if !compatible(n, *ref) {
		return openFresh(ctx, o, FreshIncompatible)
	}
	if n.Engine == Claude && !claudeConversationStored(n, ref.ID) {
		return openFresh(ctx, o, FreshUnavailable)
	}
	s, err := Resume(ctx, o, *ref)
	if err == nil {
		return s, Opened{Resumed: true}, nil
	}
	if !errors.Is(err, errConversationGone) {
		return nil, Opened{}, err
	}
	// The failed resume launched a harness. It was settled on the way out, but
	// only confirmed absence authorizes another launch in its place.
	if err = reclaimBeforeOpen(ctx, n); err != nil {
		return nil, Opened{}, err
	}
	return openFresh(ctx, o, FreshUnavailable)
}

func openFresh(ctx context.Context, o Options, reason string) (*Session, Opened, error) {
	s, err := Start(ctx, o)
	if err != nil {
		return nil, Opened{}, err
	}
	return s, Opened{Fresh: reason}, nil
}

// reclaimBeforeOpen establishes that nothing from an earlier launch is still
// running in a restricted session's directory.
//
// The lease is taken first and held throughout. Reclaim ends a harness it can
// identify, and without the lease that could be a harness another live session
// is driving right now; with it, any harness found belongs to a process that no
// longer holds the assignment.
func reclaimBeforeOpen(ctx context.Context, o Options) error {
	if o.Restriction == nil {
		return nil
	}
	dir := o.Restriction.Tools.Dir
	lease, err := holdLease(filepath.Join(dir, "session.lease"))
	if err != nil {
		return err
	}
	defer lease.Close()
	out, err := Reclaim(ctx, dir)
	if err != nil {
		return err
	}
	if !out.Confirmed {
		return ErrUnreclaimed
	}
	// Confirmed absent, so the marker has done its job. Leaving it would have a
	// later reclaim judge a group identifier the system may since have reused.
	return clearLaunchRecord(dir)
}

// claudeConversationStored reports whether the installed Claude CLI would find
// the conversation to resume. Checked against Claude Code 2.1.282's bundled
// code and a run with a disposable home: transcripts are kept at
// <config dir>/projects/<project key>/<session id>.jsonl, and a resume looks
// at the working directory's own key first, then scans every project folder
// and accepts a single match.
func claudeConversationStored(o Options, id string) bool {
	if !claudeSessionID.MatchString(id) {
		return false
	}
	projects := filepath.Join(o.Home, "projects")
	if transcriptPresent(filepath.Join(projects, claudeProjectKey(o), id+".jsonl")) {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(projects, "*", id+".jsonl"))
	found := 0
	for _, match := range matches {
		if transcriptPresent(match) {
			found++
		}
	}
	return found == 1
}

var claudeSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func transcriptPresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// claudeProjectKey names the folder the CLI keeps this working directory's
// transcripts in. The session's config directory is Options.Home: the harness
// passes it as CLAUDE_CONFIG_DIR, or leaves it unset when it is the CLI's own
// default. CLAUDE_CODE_PROJECT_DIR_NAME, inherited from this process, replaces
// the key when a config directory is set, as it does in the CLI.
func claudeProjectKey(o Options) string {
	env := environment(o)
	if envValue(env, "CLAUDE_CONFIG_DIR") != "" {
		name := strings.TrimSpace(envValue(env, "CLAUDE_CODE_PROJECT_DIR_NAME"))
		if claudeProjectName.MatchString(name) && !claudeReservedName.MatchString(name) {
			return name
		}
	}
	dir := o.WorkDir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return claudeProjectSlug(dir)
}

var (
	claudeProjectName  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	claudeReservedName = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// claudeProjectSlug is the CLI's rule: every UTF-16 unit of the resolved path
// that is not an ASCII letter or digit becomes a dash, and a result longer than
// 200 is cut to 200 and suffixed with a base-36 hash of the path.
func claudeProjectSlug(path string) string {
	units := utf16.Encode([]rune(path))
	var slug strings.Builder
	for _, unit := range units {
		if unit < 128 && (unit >= '0' && unit <= '9' || unit >= 'a' && unit <= 'z' || unit >= 'A' && unit <= 'Z') {
			slug.WriteByte(byte(unit))
			continue
		}
		slug.WriteByte('-')
	}
	if slug.Len() <= 200 {
		return slug.String()
	}
	var hash int32
	for _, unit := range units {
		hash = hash<<5 - hash + int32(unit)
	}
	magnitude := int64(hash)
	if magnitude < 0 {
		magnitude = -magnitude
	}
	return slug.String()[:200] + "-" + strconv.FormatInt(magnitude, 36)
}

func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value = v
		}
	}
	return value
}
