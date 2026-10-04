package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// Inspect reads account and quota metadata without creating a thread, sending
// a user prompt, or reading credentials. It uses a neutral temporary directory
// and the selected CLI's native login. Unsupported optional methods return
// partial data and an error; callers must inspect each snapshot independently.
func Inspect(ctx context.Context, o Options) (Inspection, error) {
	out := Inspection{Engine: o.Provider.Engine, Capabilities: CapabilitiesFor(o.Provider.Engine)}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if o.Provider.Engine.Transport() == harness.APITransport {
		return out, &UnsupportedError{Engine: o.Provider.Engine, Operation: "inspect", Code: RefusedNotOffered, Capability: harness.Support(o.Provider.Engine, harness.Account, harness.Available)}
	}
	if err := lockedLogin(o); err != nil {
		return out, err
	}
	// Ignore conversation-only configuration, including instructions and policy.
	o = Options{Provider: o.Provider}
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
	if o.Provider.Engine == harness.Grok {
		return out, &UnsupportedError{Engine: harness.Grok, Operation: "inspect", Code: RefusedNotOffered, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "inspect a Grok login with account.Inspect"}}
	}
	if o.Provider.Engine == harness.CommandCode {
		return out, &UnsupportedError{Engine: harness.CommandCode, Operation: "inspect", Code: RefusedNotOffered, Capability: harness.Support(harness.CommandCode, harness.Account, harness.Available)}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s := &Session{options: o, caps: CapabilitiesFor(o.Provider.Engine), done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	args := []string{"app-server", "--listen", "stdio://"}
	if o.Provider.Engine == harness.Claude {
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
	if o.Provider.Engine == harness.Codex {
		err = codexHandshake(ctx, w, false)
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
	out.Credits = s.Telemetry().Credits
	out.Capabilities = s.Capabilities()
	return out, errors.Join(err, quotaErr)
}

// Telemetry returns an independent copy of the most recent observations. It
// performs no I/O and remains readable after Close. There is no background
// polling: callers choose when to refresh and what freshness they require.
func (s *Session) Telemetry() Telemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Telemetry{Account: cloneAccount(s.telemetry.Account), Quota: cloneQuota(s.telemetry.Quota), Credits: cloneCredits(s.telemetry.Credits), Context: cloneContext(s.telemetry.Context)}
}

// ReadAccount reads Codex's native account endpoint, or Grok's
// _x.ai/auth/check_subscription on the session's own agent process. Claude
// reports its account in the initialize handshake; this method returns that
// observation. To inspect a changed Claude login, use Inspect (or reopen the
// session), not a second initialize on a running conversation.
func (s *Session) ReadAccount(ctx context.Context) (harness.AccountSnapshot, error) {
	if err := s.lockOp(ctx); err != nil {
		return s.Telemetry().Account, err
	}
	defer s.unlockOp()
	if err := s.telemetryOpen(); err != nil {
		return s.Telemetry().Account, err
	}
	entry := engines[s.options.Provider.Engine]
	if entry.accountFromStart {
		return s.Telemetry().Account, nil
	}
	read := entry.account
	if read == nil {
		return s.Telemetry().Account, s.notOffered("account", func(c Capabilities) harness.Capability { return c.Account })
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.transport.request(ctx, read.method, read.params())
	var a harness.AccountSnapshot
	if err == nil {
		a, err = read.parse(raw)
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
//
// The same response also refreshes Telemetry().Credits. A credit report the
// library cannot read invalidates the credits and joins ErrProtocol to the
// result, while the quota snapshot is still returned.
func (s *Session) ReadQuota(ctx context.Context) (harness.QuotaSnapshot, error) {
	if err := s.lockOp(ctx); err != nil {
		return s.Telemetry().Quota, err
	}
	defer s.unlockOp()
	if err := s.telemetryOpen(); err != nil {
		return s.Telemetry().Quota, err
	}
	read := engines[s.options.Provider.Engine].quota
	if read == nil {
		return s.Telemetry().Quota, s.notOffered("quota", func(c Capabilities) harness.Capability { return c.Quota })
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.transport.request(ctx, read.method, read.params())
	var q harness.QuotaSnapshot
	var c harness.CreditSnapshot
	creditErr := err
	if err == nil {
		q, err = read.parseQuota(raw)
		c, creditErr = read.parseCredits(raw)
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
	if creditErr == nil {
		s.telemetry.Credits = c
	} else {
		invalidate(&s.telemetry.Credits.Observation, "credits refresh failed")
	}
	recordCapability(&s.caps.Quota, err)
	q = cloneQuota(s.telemetry.Quota)
	s.mu.Unlock()
	if err != nil {
		return q, err
	}
	return q, creditErr
}

// ReadContext returns Codex's or Grok's latest streamed context observation;
// its timestamp is not refreshed by this read. Grok's is an estimate from the
// latest response's input against its model state's stated window. Claude is queried with detail=summary, avoiding
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
	read := engines[s.options.Provider.Engine].context
	if read == nil {
		return s.Telemetry().Context, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.transport.request(ctx, read.method, read.params())
	var c ContextSnapshot
	if err == nil {
		c, err = read.parse(raw)
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

// notOffered refuses a telemetry read the engine has no source for, with the
// session's own record of that capability.
func (s *Session) notOffered(operation string, field func(Capabilities) harness.Capability) error {
	s.mu.Lock()
	c := field(s.caps)
	s.mu.Unlock()
	return &UnsupportedError{Engine: s.options.Provider.Engine, Operation: operation, Code: RefusedNotOffered, Capability: c}
}

func (s *Session) telemetryOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}
func observation(source string, quality harness.Measurement) harness.Observation {
	return harness.Observation{Source: source, Quality: quality, ObservedAt: time.Now().UTC()}
}
func invalidate(o *harness.Observation, reason string) { o.Invalidated = true; o.Reason = reason }
func recordCapability(c *harness.Capability, err error) {
	if err == nil {
		*c = harness.Capability{Availability: harness.Native, Reason: "observed native telemetry"}
	} else if errors.Is(err, ErrUnsupported) {
		*c = harness.Capability{Availability: harness.Unsupported, Reason: "installed CLI does not support this telemetry method"}
	} else if c.Availability != harness.Native {
		*c = harness.Capability{Availability: harness.Unknown, Reason: "telemetry method was not successfully observed"}
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
func cloneAccount(a harness.AccountSnapshot) harness.AccountSnapshot {
	a.LoggedIn = cloneValue(a.LoggedIn)
	return a
}
func cloneCredits(c harness.CreditSnapshot) harness.CreditSnapshot {
	c.Enabled = cloneValue(c.Enabled)
	c.Unlimited = cloneValue(c.Unlimited)
	c.LimitReached = cloneValue(c.LimitReached)
	c.Balance = cloneValue(c.Balance)
	c.Used = cloneValue(c.Used)
	c.Limit = cloneValue(c.Limit)
	c.ResetsAvailable = cloneValue(c.ResetsAvailable)
	return c
}
func cloneContext(c ContextSnapshot) ContextSnapshot {
	c.UsedTokens = cloneValue(c.UsedTokens)
	c.CapacityTokens = cloneValue(c.CapacityTokens)
	c.ModelCapacityTokens = cloneValue(c.ModelCapacityTokens)
	c.UsedPercent = cloneValue(c.UsedPercent)
	c.AutoCompactAtTokens = cloneValue(c.AutoCompactAtTokens)
	return c
}
func cloneQuota(q harness.QuotaSnapshot) harness.QuotaSnapshot {
	q.UsingOverage = cloneValue(q.UsingOverage)
	q.LimitReached = cloneValue(q.LimitReached)
	q.Windows = append([]harness.QuotaWindow(nil), q.Windows...)
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

// accountRead, quotaRead and contextRead are the native requests an engine
// answers a telemetry read with. An engine without one does not offer that
// read, or, for context, serves its latest streamed observation.
type accountRead struct {
	method string
	params func() map[string]any
	parse  func(json.RawMessage) (harness.AccountSnapshot, error)
}

type quotaRead struct {
	method       string
	params       func() map[string]any
	parseQuota   func(json.RawMessage) (harness.QuotaSnapshot, error)
	parseCredits func(json.RawMessage) (harness.CreditSnapshot, error)
}

type contextRead struct {
	method string
	params func() map[string]any
	parse  func(json.RawMessage) (ContextSnapshot, error)
}

var (
	codexAccountRead = &accountRead{"account/read", func() map[string]any { return map[string]any{"refreshToken": false} }, parseCodexAccount}
	grokAccountRead  = &accountRead{grokAccountMethod, func() map[string]any { return map[string]any{} }, parseGrokAccount}
	codexQuotaRead   = &quotaRead{"account/rateLimits/read", func() map[string]any { return map[string]any{} }, parseCodexQuota, parseCodexCredits}
	// Claude's get_usage skips its local transcript and behaviour scan.
	claudeQuotaRead   = &quotaRead{"get_usage", func() map[string]any { return map[string]any{"skip_behaviors": true} }, parseClaudeQuota, parseClaudeCredits}
	claudeContextRead = &contextRead{"get_context_usage", func() map[string]any { return map[string]any{"detail": "summary"} }, parseClaudeContext}
)
