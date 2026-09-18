package session

import (
	"encoding/json"
	"math"
	"sort"
)

// Account and quota updates can arrive while idle or after a turn's result.
// They must not disappear behind the active-turn gate used for model output.
func (s *Session) accountTelemetryEvent(m map[string]json.RawMessage, ref Ref, t *Turn) bool {
	if s.options.Engine == Codex {
		switch str(m, "method") {
		case "account/rateLimits/updated":
			q, err := parseCodexQuota(m["params"])
			if err == nil {
				q.Complete = false
				q.Source = "account/rateLimits/updated"
				s.observeQuota(q, t)
			}
			return true
		case "account/updated":
			var p struct {
				Auth json.RawMessage `json:"authMode"`
				Plan string          `json:"planType"`
			}
			if json.Unmarshal(m["params"], &p) != nil || len(p.Auth) == 0 {
				return true
			}
			a := AccountSnapshot{Observation: observation("account/updated", Measured), Plan: p.Plan}
			logged := string(p.Auth) != "null"
			a.LoggedIn = &logged
			if logged && json.Unmarshal(p.Auth, &a.AuthMethod) != nil {
				return true
			}
			s.mu.Lock()
			s.telemetry.Account = a
			// Identity may have changed. Never retain the previous account's quota.
			s.telemetry.Quota = QuotaSnapshot{Observation: Observation{Reason: "account changed; refresh quota"}}
			recordCapability(&s.caps.Account, nil)
			s.mu.Unlock()
			if t != nil {
				copy := cloneAccount(a)
				s.emit(t, Event{Kind: "account", Account: &copy})
			}
			return true
		}
	} else if str(m, "type") == "rate_limit_event" {
		if id := str(m, "session_id"); id != "" && ref.ID != "" && id != ref.ID {
			return true
		}
		if str(m, "parent_tool_use_id") != "" {
			return true
		}
		if q, err := parseClaudeQuotaEvent(m["rate_limit_info"]); err == nil {
			s.observeQuota(q, t)
		}
		return true
	}
	return false
}
func (s *Session) observeQuota(q QuotaSnapshot, t *Turn) {
	s.mu.Lock()
	for i := range q.Windows {
		q.Windows[i].Source = q.Source
	}
	if !q.Complete {
		windows := map[string]QuotaWindow{}
		for _, w := range s.telemetry.Quota.Windows {
			windows[w.ID] = w
		}
		for _, w := range q.Windows {
			windows[w.ID] = w
		}
		q.Windows = nil
		ids := make([]string, 0, len(windows))
		for id := range windows {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			q.Windows = append(q.Windows, windows[id])
		}
	}
	s.telemetry.Quota = cloneQuota(q)
	recordCapability(&s.caps.Quota, nil)
	s.mu.Unlock()
	if t != nil {
		copy := cloneQuota(q)
		s.emit(t, Event{Kind: "quota", Quota: &copy})
	}
}

// observeRequestUsage records one model response's reported consumption and
// publishes it while the turn is still running. A caller watching a budget has
// nothing else to act on until the turn ends, and by then it is too late to
// interrupt.
func (s *Session) observeRequestUsage(t *Turn, u Usage) {
	if !u.Known || t == nil {
		return
	}
	t.mu.Lock()
	t.result.Observed = t.result.Observed.add(u)
	observed := t.result.Observed
	t.mu.Unlock()
	observed.Final = false
	s.emit(t, Event{Kind: "usage", Usage: &observed})
}

func (s *Session) observeContext(c ContextSnapshot, t *Turn) {
	s.mu.Lock()
	s.telemetry.Context = cloneContext(c)
	recordCapability(&s.caps.Context, nil)
	s.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		t.result.Context = cloneContext(c)
		t.mu.Unlock()
		copy := cloneContext(c)
		s.emit(t, Event{Kind: "context", Context: &copy})
	}
}
func (s *Session) invalidateContext(t *Turn, reason string) {
	s.mu.Lock()
	invalidate(&s.telemetry.Context.Observation, reason)
	c := cloneContext(s.telemetry.Context)
	s.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		t.result.Context = cloneContext(c)
		t.mu.Unlock()
		s.emit(t, Event{Kind: "context", Context: &c})
	}
}

// The latest assistant input is an observation of one model request, not a sum
// of requests. Pending outputs/tool results are not counted, hence Estimated.
func (s *Session) claudeMessageContext(t *Turn, raw json.RawMessage) {
	var r struct {
		Model string `json:"model"`
		Usage *struct {
			Input      *int64 `json:"input_tokens"`
			CacheRead  *int64 `json:"cache_read_input_tokens"`
			CacheWrite *int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Usage == nil || r.Model == "" {
		return
	}
	u := r.Usage
	if u.Input == nil || u.CacheRead == nil || u.CacheWrite == nil || *u.Input < 0 || *u.CacheRead < 0 || *u.CacheWrite < 0 {
		return
	}
	if *u.Input > math.MaxInt64-*u.CacheRead || *u.Input+*u.CacheRead > math.MaxInt64-*u.CacheWrite {
		return
	}
	used := *u.Input + *u.CacheRead + *u.CacheWrite
	c := s.Telemetry().Context
	if c.Model != r.Model {
		c = ContextSnapshot{Model: r.Model}
	}
	c.Observation = observation("assistant.message.usage", Estimated)
	c.Reason = "latest model input; pending output and tool results are not counted"
	c.UsedTokens = &used
	setContextPercent(&c)
	s.observeContext(c, t)
}
func (s *Session) claudeModelCapacity(t *Turn, raw json.RawMessage) {
	c := s.Telemetry().Context
	if c.Model == "" {
		return
	}
	var models map[string]struct {
		Capacity *int64 `json:"contextWindow"`
	}
	if json.Unmarshal(raw, &models) != nil {
		return
	}
	m, ok := models[c.Model]
	if !ok || m.Capacity == nil || *m.Capacity <= 0 {
		return
	}
	// Preserve the occupancy timestamp and invalidation state. A result's
	// cumulative token totals are never a fresh context observation.
	c.CapacityTokens = m.Capacity
	c.ModelCapacityTokens = cloneValue(m.Capacity)
	setContextPercent(&c)
	s.observeContext(c, t)
}

// Late telemetry and resumed-thread observations can arrive outside a live turn.
// Update the session snapshot without reopening a finished turn's event stream.
func (s *Session) idleTelemetry(m map[string]json.RawMessage, ref Ref, last *Turn) {
	if s.accountTelemetryEvent(m, ref, nil) || s.options.Engine != Codex {
		return
	}
	var p map[string]json.RawMessage
	if json.Unmarshal(m["params"], &p) != nil || ref.ID == "" || str(p, "threadId") != ref.ID {
		return
	}
	if id := str(p, "turnId"); last != nil && id != "" && id != last.ID() {
		return
	}
	switch str(m, "method") {
	case "thread/tokenUsage/updated":
		if c, err := parseCodexContext(p["tokenUsage"]); err == nil {
			s.observeContext(c, nil)
		}
	case "thread/compacted":
		s.invalidateContext(nil, "context compacted; awaiting a fresh observation")
	}
}
