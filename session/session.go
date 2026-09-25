package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Session struct {
	opGate    chan struct{} // serializes controls without blocking cancelled callers
	eventMu   sync.Mutex
	mu        sync.Mutex
	options   Options
	ref       Ref
	caps      Capabilities
	telemetry Telemetry
	transport wire
	active    *Turn
	closed    bool
	failure   error
	lastEvent time.Time
	tools     *toolHost
	// lifetime is the context this session was opened with. It owns the harness
	// process, so it — not a bounded control request — is what a replacement
	// turn is watched against.
	lifetime context.Context
	// identified carries the outcome of durably recording the running harness's
	// identity. A restricted session waits for it before its first inference.
	identified chan error
	done       chan struct{}
	// contextPending is the reason the caller's context is next due, if any.
	// contextGeneration moves on every mark, so a turn clears only the mark it
	// actually carried and never one a compaction set while it was starting.
	contextPending    ContextReason
	contextGeneration uint64
}

// Start opens a persistent native CLI. ctx owns its lifetime; cancelling it
// terminates the process tree. Startup performs a handshake, not inference.
func Start(ctx context.Context, o Options) (*Session, error) { return open(ctx, o, nil) }

// Resume reopens exactly the configuration described by a saved reference. An
// account label cannot detect a user logging a different account into the same
// home; the caller must update AccountIdentity when doing so.
func Resume(ctx context.Context, o Options, r Ref) (*Session, error) { return open(ctx, o, &r) }
func open(ctx context.Context, o Options, r *Ref) (*Session, error) {
	o, err := normalize(o)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if r != nil && !compatible(o, *r) {
		return nil, ErrIncompatibleResume
	}
	// The restricted runtime is prepared and proved first. Nothing below this
	// point runs with the caller's login until the harness has demonstrated,
	// against a provider that refuses to infer, that it dropped its own tools.
	l, err := prepareLaunch(ctx, o)
	if err != nil {
		return nil, err
	}
	s := &Session{options: o, ref: reference(o, ""), caps: CapabilitiesFor(o.Engine), lifetime: ctx, done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	if l != nil && l.host != nil {
		s.tools = l.host
		l.host.onRefusal = s.toolRefused
		l.host.activeTurn = func() string {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.active == nil {
				return ""
			}
			return s.active.ID()
		}
	}
	if r != nil {
		s.ref = *r
	} else if o.Engine == Claude {
		s.ref.ID = newID()
	}
	// A restricted session marks the attempt before the harness exists, then
	// names the process once it is running and contained. Doing only the second
	// would leave a window where a harness is alive and nothing on disk says so.
	// Doing only the first would leave nothing to signal. Both, in that order,
	// mean a crash at any point is either "no process" or "a process, reserved".
	var onStart func(int)
	if l != nil && l.host != nil {
		if err = recordLaunch(l.host.cfg.Dir, launchRecord{Engine: string(o.Engine), Launch: l.host.socketDir, Started: time.Now().UTC()}); err != nil {
			s.releaseTools()
			return nil, err
		}
		identified := make(chan error, 1)
		s.identified = identified
		onStart = func(pid int) {
			record := launchRecord{Engine: string(o.Engine), PID: pid, Group: pid, Launch: l.host.socketDir, Started: time.Now().UTC()}
			err := recordLaunch(l.host.cfg.Dir, record)
			identified <- err
			if err != nil {
				s.fail(err)
			}
		}
	}
	// Reader callbacks may fire before startup returns; the transport assignment
	// is protected so an early process failure cannot race Close.
	s.mu.Lock()
	w, err := newProcessWire(ctx, o, s.ref.ID, r != nil, l, onStart, s.notification, s.fail)
	s.transport = w
	s.mu.Unlock()
	if err != nil {
		s.settleFailedLaunch(l)
		s.releaseTools()
		return nil, err
	}
	// Nothing is asked of this session until its identity is durably recorded.
	// A harness whose identity could not be written is one a later process could
	// not find, so it is stopped here rather than allowed to start work.
	if err = s.awaitIdentity(ctx); err != nil {
		s.fail(err)
		s.settleFailedLaunch(l)
		return nil, s.resumeFailure(r != nil, err)
	}
	if err = s.initialize(ctx, r != nil); err != nil {
		s.fail(err)
		s.settleFailedLaunch(l)
		return nil, s.resumeFailure(r != nil, err)
	}
	if r == nil {
		s.markContext(ContextStarted)
	} else {
		s.restoreContext()
	}
	return s, nil
}

// resumeFailure marks a Claude resume whose harness exited on its own during
// startup. Checked against Claude Code 2.1.282: resuming a conversation it
// does not have writes an error result and exits with status 1, without an
// init frame or a reply to the initialize request. Only a natural exit counts;
// a harness this library had to stop is not evidence about the conversation.
func (s *Session) resumeFailure(resuming bool, err error) error {
	if !resuming || s.options.Engine != Claude || !errors.Is(err, ErrTransport) {
		return err
	}
	s.awaitReaped(context.Background())
	s.mu.Lock()
	w, _ := s.transport.(*streamWire)
	s.mu.Unlock()
	if w == nil {
		return err
	}
	var exit *ProcessError
	if errors.As(w.processError(), &exit) && exit.Code == ProcessExited {
		return fmt.Errorf("%w: %w", errConversationGone, err)
	}
	return err
}

// settleFailedLaunch clears the marker when this launch demonstrably produced
// nothing to reclaim.
//
// The marker exists so a crash between "about to spawn" and "spawned" is held
// rather than assumed away. But a launch that failed in front of us is not that
// case: a missing binary, a harness that exited during its handshake, an
// identity that could not be written — each is a known outcome, and leaving a
// marker naming no process behind would turn one repairable failure into an
// assignment that no later resume could ever unblock. So the marker is settled
// when the process is confirmed gone, and kept when it is not.
func (s *Session) settleFailedLaunch(l *launch) {
	if l == nil || l.host == nil {
		return
	}
	s.mu.Lock()
	w, _ := s.transport.(*streamWire)
	s.mu.Unlock()
	if w != nil {
		w.close()
		select {
		case <-w.reaped:
		case <-time.After(5 * time.Second):
			// Still there. That is exactly the uncertainty the marker is for.
			return
		}
	}
	record, err := readLaunchRecord(l.host.cfg.Dir)
	if err != nil {
		return
	}
	if record != nil && record.identified() {
		// A process existed. Whether it is gone is Reclaim's question, not this
		// one, and answering it here would risk clearing a live harness's marker.
		alive, aliveErr := groupAlive(record.Group)
		if aliveErr != nil || alive {
			return
		}
	}
	_ = clearLaunchRecord(l.host.cfg.Dir)
}

// awaitIdentity waits for the launch record to name the running harness. It is
// bounded by the caller's context and by the transport failing, so a harness
// that never starts does not hang the open.
func (s *Session) awaitIdentity(ctx context.Context) error {
	s.mu.Lock()
	identified := s.identified
	s.mu.Unlock()
	if identified == nil {
		return nil
	}
	select {
	case err := <-identified:
		return err
	case <-s.done:
		return s.transportFailure()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitReaped waits for the transport's process to be collected, bounded by the
// caller's context. It is best effort: a harness that will not die is exactly
// what the reclamation that follows is for.
func (s *Session) awaitReaped(ctx context.Context) {
	s.mu.Lock()
	w, _ := s.transport.(*streamWire)
	s.mu.Unlock()
	if w == nil {
		return
	}
	select {
	case <-w.reaped:
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
}

// reaped reports whether this session's harness process has been collected.
func (s *Session) reaped() bool {
	s.mu.Lock()
	w, _ := s.transport.(*streamWire)
	s.mu.Unlock()
	if w == nil {
		return true
	}
	select {
	case <-w.reaped:
		return true
	default:
		return false
	}
}

func (s *Session) transportFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	return ErrClosed
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cryptographic random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func (s *Session) initialize(ctx context.Context, resume bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.options.Engine == Codex {
		if err := codexHandshake(ctx, s.transport, s.options.Sandbox != nil); err != nil {
			return err
		}
		method := "thread/start"
		if resume {
			method = "thread/resume"
		}
		body, err := s.transport.request(ctx, method, codexThreadParams(s.options, s.options.WorkDir, resume, s.ref.ID))
		if err != nil {
			if resume && errors.Is(err, ErrRejected) {
				// Checked against codex 0.156.1: a thread it has no rollout for
				// is refused with an invalid-request error.
				return fmt.Errorf("%w: %w", errConversationGone, err)
			}
			return err
		}
		var response struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(body, &response) != nil || response.Thread.ID == "" {
			return ErrProtocol
		}
		if resume && response.Thread.ID != s.ref.ID {
			return ErrProtocol
		}
		if s.options.Sandbox != nil {
			if err = checkCodexSandbox(s.options, body); err != nil {
				return err
			}
		}
		s.mu.Lock()
		s.ref.ID = response.Thread.ID
		s.mu.Unlock()
	} else {
		body, err := s.transport.request(ctx, "initialize", map[string]any{})
		if err != nil {
			return err
		}
		s.observeClaudeAccount(body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	c := Capability{Native, "acknowledged by the installed harness"}
	if resume {
		s.caps.Resume = c
	} else {
		s.caps.Start = c
	}
	// Instructions are sent through explicit native parameters. CLI startup or
	// thread creation rejects flags/fields it cannot parse; no user-role emulation.
	if s.options.Instructions.Mode == Replace {
		s.caps.ReplaceInstructions = c
	} else if s.options.Instructions.Mode == Append {
		s.caps.AppendInstructions = c
	}
	return nil
}
func (s *Session) Ref() Ref                   { s.mu.Lock(); defer s.mu.Unlock(); return s.ref }
func (s *Session) Capabilities() Capabilities { s.mu.Lock(); defer s.mu.Unlock(); return s.caps }

// Close terminates the harness and its subprocess tree. It is idempotent.
//
// Close does not clear the launch marker: a terminated process tree is not the
// same fact as a confirmed-gone one, and this is the path a crashing caller
// never reaches anyway. Release is for a caller that has finished with an
// assignment and wants recovery to stop reserving against it.
func (s *Session) Close() { s.fail(ErrClosed) }

// Release closes the session and, once its harness is confirmed gone, removes
// the launch marker recovery would otherwise reserve against. It returns the
// reclamation outcome: a marker is only cleared on positive evidence, so a
// harness that cannot be confirmed terminated keeps its marker and its
// ErrUnreclaimed, which is what tells a later run to hold.
func (s *Session) Release(ctx context.Context) (Reclamation, error) {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	s.Close()
	if host == nil {
		// A sandboxed Codex session has no bridge to reclaim, but it ran in a
		// runtime home that may hold a refreshed login.
		if s.options.Sandbox != nil && s.options.Engine == Codex && s.options.RuntimeHome != "" {
			s.awaitReaped(ctx)
			if !s.reaped() {
				// Still running, and possibly still refreshing its login. Copying a
				// credential it may be halfway through writing is worse than
				// leaving the refresh for the next launch to share back.
				return Reclamation{Found: true}, ErrUnreclaimed
			}
			if err := writeBackCredential(s.options.Home, s.options.RuntimeHome); err != nil {
				return Reclamation{Confirmed: true}, err
			}
		}
		return Reclamation{Confirmed: true}, nil
	}
	// Wait for this session's own harness to be reaped before asking the general
	// question. Reclaim reads a process group, and a process that has exited but
	// not yet been reaped still occupies one — so asking immediately after Close
	// reports an ordinary, orderly shutdown as an unresolved survivor, which in a
	// caller's hands becomes a worker that looks stuck every time it finishes.
	s.awaitReaped(ctx)
	dir := host.cfg.Dir
	out, err := Reclaim(ctx, dir)
	if err != nil || !out.Confirmed {
		return out, err
	}
	// The harness is gone, so its home is no longer being written to. If it
	// refreshed the login, return that to the source now rather than leaving the
	// next worker to rediscover an expired one.
	if s.options.Engine == Codex && s.options.RuntimeHome != "" {
		if shareErr := writeBackCredential(s.options.Home, s.options.RuntimeHome); shareErr != nil {
			return out, shareErr
		}
	}
	return out, clearLaunchRecord(dir)
}
func (s *Session) fail(err error) { s.failTurn(nil, err) }
func (s *Session) failTurn(expected *Turn, err error) {
	s.mu.Lock()
	if expected != nil {
		if s.active != expected {
			s.mu.Unlock()
			return
		}
		expected.mu.Lock()
		finished := expected.finished
		expected.mu.Unlock()
		if finished {
			s.mu.Unlock()
			return
		}
	}
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.failure == nil && err != nil && !errors.Is(err, ErrClosed) {
		s.failure = err
	}
	close(s.done)
	t := s.active
	w := s.transport
	host := s.tools
	s.mu.Unlock()
	if t != nil {
		t.finish("failed", err)
	}
	if w != nil {
		w.close()
	}
	if host != nil {
		host.close()
	}
}
