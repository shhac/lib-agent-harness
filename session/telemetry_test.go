package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQuotaMappings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(json.RawMessage) (QuotaSnapshot, error)
		raw   string
		want  []string
		pct   []float64
	}{
		{"codex buckets", parseCodexQuota, `{"rateLimits":{"primary":{"usedPercent":99}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":1900000000},"secondary":{"usedPercent":74,"windowDurationMins":10080}},"review":{"primary":{"usedPercent":123}}}}`, []string{"codex/primary", "codex/secondary", "review/primary"}, []float64{0, 74, 123}},
		{"codex legacy", parseCodexQuota, `{"rateLimits":{"limitId":"legacy","primary":{"usedPercent":0.5}}}`, []string{"legacy/primary"}, []float64{0.5}},
		{"claude query", parseClaudeQuota, `{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":0.5,"resets_at":"2030-01-01T00:00:00Z"},"seven_day":{"utilization":100,"resets_at":null,"limit_dollars":50,"used_dollars":50},"extra_usage":{"utilization":75},"session":null}}`, []string{"five_hour", "seven_day"}, []float64{0.5, 100}},
		{"claude stream", parseClaudeQuotaEvent, `{"status":"allowed","rateLimitType":"five_hour","utilization":0.005,"unifiedWindows":{"five_hour":{"utilization":0.005,"resetsAt":1900000000},"seven_day":{"utilization":1.25}}}`, []string{"five_hour", "seven_day"}, []float64{0.5, 125}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := tc.parse(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !q.Known() || len(q.Windows) != len(tc.want) {
				t.Fatalf("bad snapshot: %+v", q)
			}
			for i, w := range q.Windows {
				if w.ID != tc.want[i] || w.UsedPercent == nil || *w.UsedPercent != tc.pct[i] {
					t.Fatalf("window mismatch: %+v", w)
				}
				if *w.RemainingPercent() != max(0, 100-tc.pct[i]) {
					t.Fatal("incorrect remainder")
				}
				if w.ObservedAt.IsZero() {
					t.Fatal("missing observation time")
				}
			}
			if tc.name == "claude query" {
				if q.Windows[0].Allowance != nil || q.Windows[1].Allowance == nil || q.Windows[1].Allowance.Unit != "USD" {
					t.Fatal("caps must only come from explicit provider units")
				}
			}
			if strings.HasPrefix(tc.name, "codex") {
				for _, w := range q.Windows {
					if w.Allowance != nil {
						t.Fatal("invented absolute cap")
					}
				}
			}
		})
	}
}

func TestUnknownIsNotZeroOrUnlimited(t *testing.T) {
	for _, raw := range []string{`{"rate_limits_available":false,"rate_limits":null}`, `{"rate_limits_available":true,"rate_limits":null}`, `{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":null,"resets_at":null}}}`} {
		q, err := parseClaudeQuota(json.RawMessage(raw))
		if err != nil || q.Known() || q.Reason == "" {
			t.Fatalf("unknown quota became known: %+v %v", q, err)
		}
	}
	if (QuotaWindow{}).RemainingPercent() != nil {
		t.Fatal("unknown quota looks free")
	}
	for _, parse := range []func(json.RawMessage) (QuotaSnapshot, error){parseCodexQuota, parseClaudeQuota, parseClaudeQuotaEvent} {
		for _, raw := range []string{`{}`, `null`, `[]`, `{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":-1,"resets_at":null}},"rateLimits":{"primary":{"usedPercent":-1}},"status":"allowed","rateLimitType":"five_hour","utilization":-1}`} {
			if _, err := parse(json.RawMessage(raw)); !errors.Is(err, ErrProtocol) {
				t.Fatalf("accepted malformed quota %s: %v", raw, err)
			}
		}
	}
}

func TestAccountMetadataAndUnknownLogin(t *testing.T) {
	a, err := parseCodexAccount(json.RawMessage(`{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":"fixture@example.test","planType":"pro","accessToken":"must-not-escape"}}`))
	if err != nil || a.Plan != "pro" || a.LoggedIn == nil || !*a.LoggedIn {
		t.Fatalf("bad Codex account: %+v %v", a, err)
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "must-not-escape") {
		t.Fatal("unrecognized private fields escaped")
	}
	a, err = parseCodexAccount(json.RawMessage(`{"requiresOpenaiAuth":true,"account":null}`))
	if err != nil || a.LoggedIn == nil || *a.LoggedIn || !a.Known() {
		t.Fatal("explicit logout lost")
	}
	a, err = parseCodexAccount(json.RawMessage(`{"requiresOpenaiAuth":false}`))
	if err != nil || a.LoggedIn != nil || a.Known() {
		t.Fatal("missing account mistaken for logged out")
	}
	a, err = parseClaudeAccount(json.RawMessage(`{"account":{"apiProvider":"firstParty"}}`))
	if err != nil || a.LoggedIn != nil || a.Plan != "" {
		t.Fatal("provider selection mistaken for login")
	}
	a, err = parseClaudeAccount(json.RawMessage(`{"account":{"email":"fixture@example.test","subscriptionType":"max","organization":"fixture-org","apiProvider":"firstParty"}}`))
	if err != nil || a.Plan != "max" || a.Organization != "fixture-org" {
		t.Fatal("Claude metadata lost")
	}
}

func TestQuotaRefreshRetainsStaleValuesAndRecovers(t *testing.T) {
	s, w := fakeSession(t, Codex)
	w.requestFn = func(method string, _ map[string]any) (json.RawMessage, error) {
		if method != "account/rateLimits/read" {
			t.Fatalf("unexpected %s", method)
		}
		return json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":70}}}`), nil
	}
	before, err := s.ReadQuota(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) { return nil, ErrRejected }
	failed, err := s.ReadQuota(testContext(t))
	if !errors.Is(err, ErrRejected) || !failed.IsStale(time.Now(), time.Hour) || !failed.Windows[0].Invalidated || !before.ObservedAt.Equal(failed.ObservedAt) || *failed.Windows[0].UsedPercent != 70 {
		t.Fatalf("failed refresh erased or freshened data: %+v %v", failed, err)
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":0}}}`), nil
	}
	after, err := s.ReadQuota(testContext(t))
	if err != nil || after.IsStale(time.Now(), time.Hour) || *after.Windows[0].UsedPercent != 0 {
		t.Fatal("refresh did not recover")
	}
	if s.Capabilities().Quota.Availability != Native {
		t.Fatal("successful capability not acknowledged")
	}
	*after.Windows[0].UsedPercent = 99
	if *s.Telemetry().Quota.Windows[0].UsedPercent != 0 {
		t.Fatal("caller mutated session quota")
	}
}

func TestUnsupportedTelemetryDoesNotCloseSession(t *testing.T) {
	s, w := fakeSession(t, Claude)
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) { return nil, ErrUnsupported }
	q, err := s.ReadQuota(testContext(t))
	if !errors.Is(err, ErrUnsupported) || q.Known() || s.Capabilities().Quota.Availability != Unsupported {
		t.Fatal("unsupported method not reported")
	}
	if _, err = s.StartTurn(testContext(t), Input{"still usable"}); err != nil {
		t.Fatal(err)
	}
	finishClaude(s, false)
}

func TestIdleQuotaEventsAndAccountChange(t *testing.T) {
	s, _ := fakeSession(t, Codex)
	notify(s, `{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":12}}}}`)
	notify(s, `{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"review","primary":{"usedPercent":20}}}}`)
	if q := s.Telemetry().Quota; len(q.Windows) != 2 || q.Complete {
		t.Fatalf("idle updates lost or claimed complete: %+v", q)
	}
	notify(s, `{"method":"account/updated","params":{"authMode":null,"planType":null}}`)
	a := s.Telemetry()
	if a.Account.LoggedIn == nil || *a.Account.LoggedIn || a.Quota.Known() || len(a.Quota.Windows) != 0 {
		t.Fatal("account change retained prior allowance")
	}
	c, _ := fakeSession(t, Claude)
	notify(c, `{"type":"rate_limit_event","session_id":"other","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","utilization":0.5}}`)
	if c.Telemetry().Quota.Known() {
		t.Fatal("foreign quota accepted")
	}
	notify(c, `{"type":"rate_limit_event","session_id":"session-1","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","utilization":0.5}}`)
	if q := c.Telemetry().Quota; len(q.Windows) != 1 || *q.Windows[0].UsedPercent != 50 {
		t.Fatal("idle Claude update lost")
	}
}

func TestCodexContextUsesActiveSizeAndSurvivesCompaction(t *testing.T) {
	s, _, turn := startedCodexTurn(t)
	frame := `{"method":"thread/tokenUsage/updated","params":{"threadId":"session-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":800,"cachedInputTokens":700,"outputTokens":100,"reasoningOutputTokens":0,"totalTokens":900},"total":{"inputTokens":800000,"cachedInputTokens":700000,"outputTokens":100000,"reasoningOutputTokens":0,"totalTokens":900000},"modelContextWindow":1000}}}`
	notify(s, frame)
	c := s.Telemetry().Context
	if c.UsedTokens == nil || *c.UsedTokens != 900 || *c.UsedPercent != 90 || c.Quality != Measured {
		t.Fatalf("billing mistaken for context: %+v", c)
	}
	notify(s, strings.Replace(frame, `"turn-1"`, `"old-turn"`, 1))
	if !c.ObservedAt.Equal(s.Telemetry().Context.ObservedAt) {
		t.Fatal("foreign turn changed context")
	}
	notify(s, `{"method":"thread/compacted","params":{"threadId":"session-1","turnId":"turn-1"}}`)
	if !s.Telemetry().Context.IsStale(time.Now(), time.Hour) {
		t.Fatal("compaction left old context fresh")
	}
	notify(s, strings.Replace(frame, `"totalTokens":900}`, `"totalTokens":100}`, 1))
	c = s.Telemetry().Context
	if c.Invalidated || *c.UsedTokens != 100 {
		t.Fatal("context must shrink after compaction")
	}
	notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
	r, err := turn.Wait(testContext(t))
	if err != nil || *r.Context.UsedTokens != 100 {
		t.Fatal("result lost context")
	}
	var contexts int
	for e := range turn.Events() {
		if e.Context != nil {
			contexts++
			if e.Context.UsedTokens != nil {
				*e.Context.UsedTokens = 999
			}
		}
	}
	if contexts < 3 || *s.Telemetry().Context.UsedTokens != 100 {
		t.Fatal("missing or mutable context events")
	}
	read, err := s.ReadContext(testContext(t))
	if err != nil || !read.ObservedAt.Equal(c.ObservedAt) {
		t.Fatal("read freshened observation")
	}
}

func TestClaudeContextLatestPromptNotCumulativeOrSubagent(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	msg := `{"type":"assistant","session_id":"session-1","message":{"id":"m","model":"fixture-model","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20},"content":[]}}`
	notify(s, msg)
	c := s.Telemetry().Context
	if c.UsedTokens == nil || *c.UsedTokens != 130 || c.CapacityTokens != nil || c.Quality != Estimated {
		t.Fatalf("wrong input estimate: %+v", c)
	}
	notify(s, strings.Replace(msg, `"type":"assistant"`, `"type":"assistant","parent_tool_use_id":"child"`, 1))
	if !c.ObservedAt.Equal(s.Telemetry().Context.ObservedAt) {
		t.Fatal("subagent changed context")
	}
	notify(s, strings.Replace(msg, `"input_tokens":10`, `"input_tokens":5`, 1))
	notify(s, `{"type":"result","subtype":"success","session_id":"session-1","usage":{"input_tokens":900000,"output_tokens":10},"modelUsage":{"other-model":{"contextWindow":1000000},"fixture-model":{"contextWindow":1000,"inputTokens":900000}}}`)
	r, err := turn.Wait(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if *r.Context.UsedTokens != 125 || *r.Context.CapacityTokens != 1000 || *r.Context.UsedPercent != 12.5 {
		t.Fatalf("wrong capacity/occupancy: %+v", r.Context)
	}
}

func TestClaudeContextQueryIsSummaryAndCompactionInvalidates(t *testing.T) {
	s, w := fakeSession(t, Claude)
	w.requestFn = func(method string, p map[string]any) (json.RawMessage, error) {
		if method != "get_context_usage" || !reflect.DeepEqual(p, map[string]any{"detail": "summary"}) {
			t.Fatalf("unexpected context query %s %+v", method, p)
		}
		return json.RawMessage(`{"totalTokens":200,"maxTokens":1000,"rawMaxTokens":1200,"autoCompactThreshold":850,"model":"fixture-model","apiUsage":{"input_tokens":190}}`), nil
	}
	c, err := s.ReadContext(testContext(t))
	if err != nil || c.Quality != Estimated || *c.UsedPercent != 20 || *c.ModelCapacityTokens != 1200 || *c.AutoCompactAtTokens != 850 {
		t.Fatalf("bad context summary %+v %v", c, err)
	}
	_, err = s.StartTurn(testContext(t), Input{"next"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"system","subtype":"compact_boundary","session_id":"session-1"}`)
	if !s.Telemetry().Context.Invalidated {
		t.Fatal("compaction failed to invalidate context")
	}
	finishClaude(s, false)
	if !s.Telemetry().Context.Invalidated {
		t.Fatal("result counts incorrectly refreshed context")
	}
}

func TestFreshnessAndCancelledInspection(t *testing.T) {
	o := observation("fixture", Measured)
	if o.IsStale(o.ObservedAt, time.Second) || !o.IsStale(o.ObservedAt.Add(2*time.Second), time.Second) || !(Observation{}).IsStale(time.Now(), 0) {
		t.Fatal("freshness mismatch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, Options{Engine: Codex, Binary: "must-not-launch"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCodexIdleContextAfterCompletionAndBeforeFirstTurn(t *testing.T) {
	s, w := fakeSession(t, Codex)
	event := `{"method":"thread/tokenUsage/updated","params":{"threadId":"session-1","turnId":"old-turn","tokenUsage":{"last":{"totalTokens":500},"modelContextWindow":1000}}}`
	notify(s, event)
	if c := s.Telemetry().Context; c.UsedTokens == nil || *c.UsedTokens != 500 {
		t.Fatal("resumed context was dropped")
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
	}
	turn, err := s.StartTurn(testContext(t), Input{"next"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if _, err = turn.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
	notify(s, strings.Replace(event, `"old-turn"`, `"turn-1"`, 1))
	if s.Telemetry().Context.Invalidated {
		t.Fatal("late context notification was dropped")
	}
	before := s.Telemetry().Context
	notify(s, event)
	if !s.Telemetry().Context.ObservedAt.Equal(before.ObservedAt) {
		t.Fatal("old turn overwrote current context")
	}
}

func TestTelemetryBuffersBeforeCodexTurnIDIsKnown(t *testing.T) {
	s, w := fakeSession(t, Codex)
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		notify(s, `{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":10}}}}`)
		return json.RawMessage(`{"turn":{"id":"server-turn"}}`), nil
	}
	turn, err := s.StartTurn(testContext(t), Input{"next"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-turn.Events():
		if e.Kind != "quota" || e.TurnID != "server-turn" {
			t.Fatalf("wrong telemetry turn ID: %+v", e)
		}
	case <-testContext(t).Done():
		t.Fatal("buffered quota not replayed")
	}
}

func TestClaudeServerNamedModelWindow(t *testing.T) {
	q, err := parseClaudeQuota(json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"model_scoped":[{"display_name":"Future model","utilization":21,"resets_at":null}]}}`))
	if err != nil || len(q.Windows) != 1 || q.Windows[0].Scope != "Future model" || *q.Windows[0].UsedPercent != 21 || *q.Windows[0].WindowMinutes != 10080 {
		t.Fatalf("lost model allowance: %+v %v", q, err)
	}
}
