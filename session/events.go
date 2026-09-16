package session

// The engine-agnostic dispatch layer: deciding which turn a frame belongs to,
// buffering it while a turn is still starting, and the shared text/emit
// accumulation. The per-engine dialects are in events_codex.go and
// events_claude.go.

import (
	"encoding/json"
)

func (s *Session) notification(m map[string]json.RawMessage) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.notificationLocked(m)
}
func (s *Session) notificationLocked(m map[string]json.RawMessage) {
	s.mu.Lock()
	t := s.active
	ref := s.ref
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	if t == nil {
		s.idleTelemetry(m, ref, nil)
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		s.idleTelemetry(m, ref, t)
		return
	}
	if t.starting {
		size := len(mustMarshal(m))
		if len(t.pending) >= s.options.EventBuffer {
			t.mu.Unlock()
			s.fail(ErrBackpressure)
			return
		}
		if t.pendingBytes+size > MaxFrameBytes {
			t.mu.Unlock()
			s.fail(ErrOutputLimit)
			return
		}
		t.pending = append(t.pending, m)
		t.pendingBytes += size
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	if s.accountTelemetryEvent(m, ref, t) {
		return
	}
	if s.options.Engine == Codex {
		s.codexEvent(t, ref, m)
	} else {
		s.claudeEvent(t, ref, m)
	}
}
func (s *Session) emit(t *Turn, e Event) {
	if err := t.emit(e); err != nil {
		s.fail(err)
	}
}
func (s *Session) text(t *Turn, item, text string, replace bool) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	if replace || t.textItem != item {
		t.result.Text = ""
		t.textItem = item
	}
	if len(t.result.Text)+len(text) > s.options.MaxTextBytes {
		t.mu.Unlock()
		s.fail(ErrOutputLimit)
		return
	}
	t.result.Text += text
	t.mu.Unlock()
	kind := "text_delta"
	if replace {
		kind = "text"
	}
	s.emit(t, Event{Kind: kind, ItemID: item, Text: text})
}
func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }
