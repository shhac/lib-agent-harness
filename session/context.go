package session

// The caller's context: asking for it when a conversation is new or has been
// compacted, delivering it ahead of the next turn's input, and remembering that
// it is due across a restart.
//
// The harness sees compaction boundaries on both engines' streams; the caller
// knows what its domain looks like now. So the harness says when, and the
// caller says what — the harness never replays history of its own.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// contextDelivery is what one turn carries: the reason it was due, the mark it
// answers, and the caller's text for it.
type contextDelivery struct {
	reason     ContextReason
	generation uint64
	text       string
}

func (d contextDelivery) wrap(input string) string {
	if d.text == "" {
		return input
	}
	return `<caller-context reason="` + string(d.reason) + "\">\n" + d.text + "\n</caller-context>\n\n" + input
}

// callerContext asks the caller for its context when a reason is pending. It
// runs under the operation gate, so no other control can start a turn between
// this answer and the turn that carries it.
func (s *Session) callerContext(ctx context.Context) (contextDelivery, error) {
	s.mu.Lock()
	delivery := contextDelivery{reason: s.contextPending, generation: s.contextGeneration}
	idle := s.claimIdleLocked()
	s.mu.Unlock()
	handler := s.options.Context
	if delivery.reason == "" || handler == nil || idle != nil {
		// A turn that cannot start fails on its own terms below; asking the
		// caller for context it would not carry only costs the caller.
		return delivery, nil
	}
	text, err := handler(ctx, delivery.reason)
	if err != nil {
		return contextDelivery{}, fmt.Errorf("caller context for a %s conversation: %w", delivery.reason, err)
	}
	delivery.text = text
	return delivery, nil
}

// contextDelivered clears the reason a turn carried once the harness has
// accepted that turn, unless a newer mark arrived while it was starting.
func (s *Session) contextDelivered(d contextDelivery) {
	if d.reason == "" {
		return
	}
	s.mu.Lock()
	if s.contextGeneration != d.generation {
		s.mu.Unlock()
		return
	}
	s.contextPending = ""
	err := s.persistContextLocked()
	s.mu.Unlock()
	s.reportMarker(err)
}

// markContext records that the caller's context is due. A compaction replaces
// a pending "started": it happens inside an accepted turn, and that turn carried
// whatever was pending when it began.
func (s *Session) markContext(reason ContextReason) {
	s.mu.Lock()
	s.contextGeneration++
	s.contextPending = reason
	err := s.persistContextLocked()
	s.mu.Unlock()
	s.reportMarker(err)
}

// restoreContext reads back a reason a resumed restricted session left due.
func (s *Session) restoreContext() {
	path := contextMarkerPath(s.options)
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	switch reason := ContextReason(raw); reason {
	case ContextStarted, ContextCompacted:
		s.mu.Lock()
		s.contextPending = reason
		s.contextGeneration++
		s.mu.Unlock()
	}
}

// persistContextLocked mirrors the pending reason to a restricted session's
// directory. Call it with s.mu held.
func (s *Session) persistContextLocked() error {
	path := contextMarkerPath(s.options)
	if path == "" {
		return nil
	}
	if s.contextPending != "" {
		return writePrivate(path, []byte(s.contextPending))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// reportMarker tells the caller a marker could not be kept on disk. It still
// holds in memory, so it is lost only if the process also restarts: worth
// telling the caller about, not worth failing a turn over.
func (s *Session) reportMarker(err error) {
	if err == nil || s.options.OnDiagnostic == nil {
		return
	}
	s.options.OnDiagnostic(Diagnostic{Engine: string(s.options.Engine), Stage: "context_marker", Code: "context_marker_unwritten", At: time.Now().UTC()})
}

// contextMarkerPath is where a restricted session keeps its pending reason,
// beside its launch record. An ordinary session has no private directory.
func contextMarkerPath(o Options) string {
	if o.Restriction == nil {
		return ""
	}
	return filepath.Join(o.Restriction.Tools.Dir, "context.pending")
}
