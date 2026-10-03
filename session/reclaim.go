package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// reclaimUnderLease takes the assignment lease and then reclaims dir, returning
// the lease still held when nothing from an earlier launch remains.
//
// The lease comes first. Reclaim ends a harness it can identify, and without
// the lease that could be a harness another live session is driving right now;
// with it, any harness found belongs to a process that no longer holds the
// assignment. A lease another session holds is returned as ErrLeaseHeld.
func reclaimUnderLease(ctx context.Context, dir string) (*os.File, Reclamation, error) {
	lease, err := holdLease(leasePath(dir))
	if err != nil {
		return nil, Reclamation{}, err
	}
	out, err := Reclaim(ctx, dir)
	if err == nil && !out.Confirmed {
		err = ErrUnreclaimed
	}
	if err != nil {
		_ = lease.Close()
		return nil, out, err
	}
	return lease, out, nil
}
