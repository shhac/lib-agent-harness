package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Inspect reads account and quota metadata without creating a thread, sending
// a user prompt, or reading credentials. It uses a neutral temporary directory
// and the selected CLI's native login. Unsupported optional methods return
// partial data and an error; callers must inspect each snapshot independently.
func Inspect(ctx context.Context, o Options) (Inspection, error) {
	out := Inspection{Engine: o.Engine, Capabilities: CapabilitiesFor(o.Engine)}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	// Ignore conversation-only configuration, including instructions and policy.
	o = Options{Engine: o.Engine, Binary: o.Binary, Home: o.Home}
	dir, err := os.MkdirTemp("", "agent-harness-inspect-")
	if err != nil {
		return out, ErrTransport
	}
	defer os.RemoveAll(dir)
	o.WorkDir = dir
	o, err = normalize(o)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s := &Session{options: o, caps: CapabilitiesFor(o.Engine), done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	args := []string{"app-server", "--listen", "stdio://"}
	if o.Engine == Claude {
		args = []string{"--safe-mode", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	}
	s.mu.Lock()
	w, err := newProcessWireArgs(ctx, o, args, nil, nil, s.notification, s.fail)
	s.transport = w
	s.mu.Unlock()
	if err != nil {
		return out, err
	}
	defer func() { s.Close(); <-w.reaped }()
	if o.Engine == Codex {
		err = codexHandshake(ctx, w)
	} else {
		var body json.RawMessage
		if body, err = w.request(ctx, "initialize", map[string]any{}); err == nil {
			s.observeClaudeAccount(body)
		}
	}
	if err != nil {
		return out, err
	}
	out.Account, err = s.ReadAccount(ctx)
	var quotaErr error
	out.Quota, quotaErr = s.ReadQuota(ctx)
	out.Capabilities = s.Capabilities()
	return out, errors.Join(err, quotaErr)
}

// Telemetry returns an independent copy of the most recent observations. It
// performs no I/O and remains readable after Close. There is no background
// polling: callers choose when to refresh and what freshness they require.
func (s *Session) Telemetry() Telemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Telemetry{Account: cloneAccount(s.telemetry.Account), Quota: cloneQuota(s.telemetry.Quota), Context: cloneContext(s.telemetry.Context)}
}

// ReadAccount reads Codex's native account endpoint. Claude reports its account
// in the initialize handshake; this method returns that observation. To inspect
// a changed Claude login, use Inspect (or reopen the session), not a second
// initialize on a running conversation.
func (s *Session) ReadAccount(ctx context.Context) (AccountSnapshot, error) {
	if err := s.lockOp(ctx); err != nil {
		return s.Telemetry().Account, err
	}
	defer s.unlockOp()
	if err := s.telemetryOpen(); err != nil {
		return s.Telemetry().Account, err
	}
	if s.options.Engine == Claude {
		a := s.Telemetry().Account
		return a, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.transport.request(ctx, "account/read", map[string]any{"refreshToken": false})
	var a AccountSnapshot
	if err == nil {
		a, err = parseCodexAccount(raw)
	}
	s.mu.Lock()
	if err == nil {
		s.telemetry.Account = a
	} else {
		invalidate(&s.telemetry.Account.Observation, "account refresh failed")
	}
	recordCapability(&s.caps.Account, err)
	a = cloneAccount(s.telemetry.Account)
	s.mu.Unlock()
	return a, err
}

// ReadQuota asks the native CLI for current allowance windows. Claude's
// experimental get_usage method skips its local transcript/behavior scan.
// Neither path performs inference or reads native credential storage itself.
func (s *Session) ReadQuota(ctx context.Context) (QuotaSnapshot, error) {
	if err := s.lockOp(ctx); err != nil {
		return s.Telemetry().Quota, err
	}
	defer s.unlockOp()
	if err := s.telemetryOpen(); err != nil {
		return s.Telemetry().Quota, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	method, params := "account/rateLimits/read", map[string]any{}
	if s.options.Engine == Claude {
		method = "get_usage"
		params["skip_behaviors"] = true
	}
	raw, err := s.transport.request(ctx, method, params)
	var q QuotaSnapshot
	if err == nil {
		if s.options.Engine == Codex {
			q, err = parseCodexQuota(raw)
		} else {
			q, err = parseClaudeQuota(raw)
		}
	}
	s.mu.Lock()
	if err == nil {
		s.telemetry.Quota = q
	} else {
		invalidate(&s.telemetry.Quota.Observation, "quota refresh failed")
		for i := range s.telemetry.Quota.Windows {
			invalidate(&s.telemetry.Quota.Windows[i].Observation, "quota refresh failed")
		}
	}
	recordCapability(&s.caps.Quota, err)
	q = cloneQuota(s.telemetry.Quota)
	s.mu.Unlock()
	return q, err
}

// ReadContext returns Codex's latest streamed context observation; its timestamp
// is not refreshed by this read. Claude is queried with detail=summary, avoiding
// per-category token-count API calls. That summary includes local estimates and
// is labelled Estimated. A newly opened Codex session is unknown until it emits
// a tokenUsage notification. No compaction or model request is performed.
func (s *Session) ReadContext(ctx context.Context) (ContextSnapshot, error) {
	if err := s.lockOp(ctx); err != nil {
		return s.Telemetry().Context, err
	}
	defer s.unlockOp()
	if err := s.telemetryOpen(); err != nil {
		return s.Telemetry().Context, err
	}
	if s.options.Engine == Codex {
		return s.Telemetry().Context, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.transport.request(ctx, "get_context_usage", map[string]any{"detail": "summary"})
	var c ContextSnapshot
	if err == nil {
		c, err = parseClaudeContext(raw)
	}
	s.mu.Lock()
	if err == nil {
		s.telemetry.Context = c
	} else {
		invalidate(&s.telemetry.Context.Observation, "context refresh failed")
	}
	recordCapability(&s.caps.Context, err)
	c = cloneContext(s.telemetry.Context)
	s.mu.Unlock()
	return c, err
}

func (s *Session) telemetryOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}
func observation(source string, quality Measurement) Observation {
	return Observation{Source: source, Quality: quality, ObservedAt: time.Now().UTC()}
}
func invalidate(o *Observation, reason string) { o.Invalidated = true; o.Reason = reason }
func recordCapability(c *Capability, err error) {
	if err == nil {
		*c = Capability{Native, "observed native telemetry"}
	} else if errors.Is(err, ErrUnsupported) {
		*c = Capability{Unsupported, "installed CLI does not support this telemetry method"}
	} else if c.Availability != Native {
		*c = Capability{Unknown, "telemetry method was not successfully observed"}
	}
}
func (s *Session) observeClaudeAccount(raw json.RawMessage) {
	a, err := parseClaudeAccount(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.telemetry.Account = a
		recordCapability(&s.caps.Account, nil)
	} else {
		s.telemetry.Account.Observation.Reason = "CLI did not report account metadata in its handshake"
	}
}
func cloneValue[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
func cloneAccount(a AccountSnapshot) AccountSnapshot { a.LoggedIn = cloneValue(a.LoggedIn); return a }
func cloneContext(c ContextSnapshot) ContextSnapshot {
	c.UsedTokens = cloneValue(c.UsedTokens)
	c.CapacityTokens = cloneValue(c.CapacityTokens)
	c.ModelCapacityTokens = cloneValue(c.ModelCapacityTokens)
	c.UsedPercent = cloneValue(c.UsedPercent)
	c.AutoCompactAtTokens = cloneValue(c.AutoCompactAtTokens)
	return c
}
func cloneQuota(q QuotaSnapshot) QuotaSnapshot {
	q.UsingOverage = cloneValue(q.UsingOverage)
	q.Windows = append([]QuotaWindow(nil), q.Windows...)
	for i := range q.Windows {
		w := &q.Windows[i]
		w.UsedPercent = cloneValue(w.UsedPercent)
		w.WindowMinutes = cloneValue(w.WindowMinutes)
		w.ResetsAt = cloneValue(w.ResetsAt)
		w.Allowance = cloneValue(w.Allowance)
		if w.Allowance != nil {
			w.Allowance.Used = cloneValue(w.Allowance.Used)
		}
	}
	return q
}
