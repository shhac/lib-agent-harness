package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Recovering from a crash of the process that launched a harness is a question
// about two different things, and conflating them is how a second worker gets
// started against a live account.
//
//   - Is the harness subtree still running? Only the operating system can
//     answer that, and it answers it about a process group.
//   - Is that group ours? A recorded integer is not an answer: process and
//     group identifiers are reused. Something alive has to say so.
//
// So absence is established from the group, and identity is established from a
// live bridge that names the launch it belongs to. Anything else is unresolved,
// and unresolved means hold the work, not start another one.

// launchRecord is written twice: once before the harness is spawned, naming no
// process, and again once it is running and contained, naming its group.
//
// The first write is the one that matters for safety. A record created after
// the spawn has a window — however short — in which a process exists and
// nothing on disk says so, and a crash inside that window would look exactly
// like an assignment that never started. So the marker goes down first, and a
// marker with no process identity means "a harness may exist and its identity
// was never recorded", which is a state to hold rather than to resume past.
type launchRecord struct {
	Engine  string    `json:"engine"`
	PID     int       `json:"pid"`
	Group   int       `json:"group"`
	Launch  string    `json:"launch"`
	Started time.Time `json:"started"`
}

// identified reports whether this record names a process that can be checked.
func (r launchRecord) identified() bool { return r.Group > 1 && r.PID > 1 }

// ErrUncertainLaunch reports a launch marker with no recorded process identity.
// A harness may or may not have started, and nothing can establish which; the
// assignment is reserved for inspection rather than resumed or restarted.
var ErrUncertainLaunch = errors.New("harness launch was recorded without a process identity")

// owner is what a live bridge writes into the lock it holds. Launch ties it to
// one specific launch record; a bridge from some other session, or a stale file
// left by one, will not match.
type owner struct {
	Group  int       `json:"group"`
	PID    int       `json:"pid"`
	Parent int       `json:"parent"`
	Launch string    `json:"launch"`
	HeldAt time.Time `json:"held_at"`
}

// Reclamation describes what a recovery attempt established. Confirmed is the
// only field that authorizes starting work for this assignment again.
type Reclamation struct {
	// Found reports that a process group from the recorded launch still exists.
	Found bool
	// Confirmed reports positive evidence that nothing from the launch remains:
	// either no group was ever recorded, or the group no longer exists.
	Confirmed bool
	// Terminated reports that this call signalled the group.
	Terminated bool
	// Group is the recorded process group, when there was one.
	Group int
}

// ErrUnreclaimed reports a harness that could not be confirmed gone. Treat the
// work as reserved and needing inspection: this is not permission to start
// another worker, and it is not permission to signal a process whose identity
// was never established.
var ErrUnreclaimed = errors.New("harness subtree could not be confirmed terminated")

// Reclaim establishes whether a harness launched from dir is still running, and
// ends it when it can prove the process group is the one it launched.
//
// A free bridge lock proves only that no bridge holds it. A harness can outlive
// its tool server, restart it, or sit in inference with none running, so the
// lock is never read as absence. Absence comes from the process group itself.
// Identity, which is what makes signalling safe, comes from a live bridge that
// names the same launch as the recorded one.
func Reclaim(ctx context.Context, dir string) (Reclamation, error) {
	var out Reclamation
	if !restrictedPlatform() {
		return out, &CapabilityError{Code: CapabilityUnsupportedPlatform, Phase: BeforeLaunch}
	}
	record, err := readLaunchRecord(dir)
	if err != nil {
		// A launch happened, and what it left running cannot be told. Reserve it.
		return out, errors.Join(ErrUnreclaimed, err)
	}
	if record == nil {
		// Nothing was ever launched here. That is positive evidence, not a guess.
		out.Confirmed = true
		return out, nil
	}
	if !record.identified() {
		// The marker was written and the identity never was. Something may be
		// running; this cannot tell. Reserve it.
		out.Found = true
		return out, errors.Join(ErrUnreclaimed, ErrUncertainLaunch)
	}
	out.Group = record.Group
	alive, err := groupAlive(record.Group)
	if err != nil {
		return out, errors.Join(ErrUnreclaimed, err)
	}
	if !alive {
		out.Confirmed = true
		return out, nil
	}
	out.Found = true
	// Something occupies the recorded group. Before signalling it, require a
	// living bridge to identify it as this launch; a reused group identifier
	// belonging to unrelated work must never be killed on a stored integer.
	held, err := readBridgeLock(lockPath(dir))
	if err != nil {
		return out, errors.Join(ErrUnreclaimed, err)
	}
	if held == nil || held.Launch == "" || held.Launch != record.Launch || held.Group != record.Group {
		return out, ErrUnreclaimed
	}
	if err = terminateGroup(record.Group, record.PID); err != nil {
		return out, errors.Join(ErrUnreclaimed, err)
	}
	out.Terminated = true
	// A signalled group takes a moment to be reaped. Confirm from the group, not
	// from the lock.
	for attempt := 0; attempt < 50; attempt++ {
		alive, err = groupAlive(record.Group)
		if err != nil {
			return out, errors.Join(ErrUnreclaimed, err)
		}
		if !alive {
			out.Confirmed = true
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return out, ErrUnreclaimed
}

// recordLaunch persists what was started, before it can produce any effect. A
// crash between this write and the process starting leaves a record for a group
// that never existed, which recovery reports as confirmed-absent — the safe way
// round.
func recordLaunch(dir string, r launchRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return errors.New("harness launch record could not be encoded")
	}
	return writePrivate(launchPath(dir), raw)
}

func readLaunchRecord(dir string) (*launchRecord, error) {
	raw, err := os.ReadFile(launchPath(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("harness launch record could not be read")
	}
	var record launchRecord
	if json.Unmarshal(raw, &record) != nil {
		return nil, errors.New("harness launch record is unreadable")
	}
	return &record, nil
}

// clearLaunchRecord removes the marker once a session has been closed cleanly
// and its harness is known to be gone. Leaving it behind would make the next
// run reserve itself against a process that ended normally.
func clearLaunchRecord(dir string) error {
	err := os.Remove(launchPath(dir))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("harness launch record could not be cleared")
}

// RunBridge relays one harness's tool protocol to the session that configured
// it, and holds the lock that lets recovery identify the launch it belongs to.
// It is the whole implementation a caller's bridge command needs; it carries no
// policy, executes nothing and interprets nothing it relays.
//
// The channel, its credential and the lock are named by the environment
// variables the session set for this process. The credential is read from an
// owner-only file and sent once; it never appears in arguments, in relayed
// content, or in anything a model can ask for.
func RunBridge(ctx context.Context, in io.Reader, out io.Writer) error {
	socket := os.Getenv(BridgeSocketEnv)
	secretFile := os.Getenv(BridgeSecretEnv)
	lockFile := os.Getenv(BridgeLockEnv)
	if socket == "" || secretFile == "" || lockFile == "" {
		return errors.New("bridge must be started by a harness session")
	}
	lock, err := holdBridgeLock(lockFile, filepath.Dir(socket))
	if err != nil {
		return err
	}
	defer lock.Close()
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return errors.New("bridge credential is unavailable")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return errors.New("bridge could not reach its session")
	}
	defer conn.Close()
	hello, err := json.Marshal(map[string]string{"secret": strings.TrimSpace(string(secret))})
	if err != nil {
		return errors.New("bridge could not present its credential")
	}
	if _, err = conn.Write(append(hello, '\n')); err != nil {
		return errors.New("bridge could not present its credential")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	go func() {
		_, _ = io.Copy(conn, in)
		// Closing the write side lets the session observe the harness's end of
		// the stream instead of waiting on a half-open connection.
		if half, ok := conn.(*net.UnixConn); ok {
			_ = half.CloseWrite()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(out, conn)
	}()
	// The harness's end of the stream is the authority on when relaying is over.
	// A cancelled context ends it too, without waiting on a read from a harness
	// that may never write again.
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}
