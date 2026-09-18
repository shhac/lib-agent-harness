package session

// The engine-agnostic dispatch layer: deciding which turn a frame belongs to,
// buffering it while a turn is still starting, and the shared text/emit
// accumulation. The per-engine dialects are in events_codex.go and
// events_claude.go.

import (
	"encoding/json"
	"time"
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
	s.lastEvent = time.Now().UTC()
	s.mu.Unlock()
	if closed {
		return
	}
	if s.options.Engine == Claude && s.observeClaudeInit(m) {
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

// observeClaudeInit cross-checks the tool surface the harness says it enabled,
// on the frame it emits at startup and therefore before any prompt. The
// pre-launch probe is what establishes the restriction; this catches a harness
// whose behaviour differs between a probe and a credentialed run, still without
// any inference having happened.
func (s *Session) observeClaudeInit(m map[string]json.RawMessage) bool {
	if str(m, "type") != "system" || str(m, "subtype") != "init" {
		return false
	}
	if s.options.Restriction == nil {
		return false
	}
	var frame struct {
		Tools []string `json:"tools"`
	}
	if json.Unmarshal(mustMarshal(m), &frame) != nil {
		s.recordRestriction(&CapabilityError{Engine: string(Claude), Code: CapabilityProbeUnreadable, Phase: BeforeFirstPrompt})
		return true
	}
	server := s.options.Restriction.Tools.Server
	observed := make([]string, 0, len(frame.Tools))
	for _, name := range frame.Tools {
		observed = append(observed, normalizeWireTool(name, server))
	}
	s.recordRestriction(compareTools(string(Claude), BeforeFirstPrompt, toolNames(s.options.Restriction.Tools.Tools), observed))
	return true
}

// recordRestriction stores the outcome of the startup cross-check. A mismatch
// ends the session: a harness with tools the caller did not authorize is a
// disclosure path, and the right response is to stop, not to note it.
func (s *Session) recordRestriction(failure *CapabilityError) {
	if failure != nil {
		s.mu.Lock()
		s.caps.RestrictTools = Capability{Unsupported, "installed harness advertised a different tool surface"}
		s.mu.Unlock()
		s.fail(failure)
		return
	}
	s.mu.Lock()
	s.caps.RestrictTools = Capability{Native, "installed harness advertised exactly the configured tools"}
	s.mu.Unlock()
}
