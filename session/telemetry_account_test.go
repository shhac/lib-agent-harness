package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// Verified wire data, with identifiers replaced: codex-cli 0.156.1's
// account/rateLimits/read result and Claude 2.1.283's get_usage response.
const (
	codexRateLimitsFixture = `{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":2,"windowDurationMins":10080,"resetsAt":1791113081},"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"individualLimit":null,"spendControlReached":false,"planType":"prolite","rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":2,"windowDurationMins":10080,"resetsAt":1791113081},"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"individualLimit":null,"spendControlReached":false,"planType":"prolite","rateLimitReachedType":null}},"rateLimitResetCredits":{"availableCount":2,"credits":[{"id":"fixture-credit","resetType":"codexRateLimits","status":"available","grantedAt":1788582130,"expiresAt":1791174130,"title":"Full reset","description":"fixture description"}]},"accountId":"fixture-account","rateLimitUpsell":null}`

	claudeUsageFixture = `{"session":{"total_cost_usd":0},"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":11,"resets_at":"2026-09-27T19:59:59.718097+00:00","limit_dollars":null,"used_dollars":null,"remaining_dollars":null,"locked_reason":null},"seven_day":{"utilization":3,"resets_at":"2026-10-04T14:59:59.718121+00:00","limit_dollars":null,"used_dollars":null,"remaining_dollars":null,"locked_reason":null},"seven_day_opus":null,"iguana_necktie":{"utilization":0,"resets_at":"2026-11-05T07:59:00+00:00","limit_dollars":250,"used_dollars":0,"remaining_dollars":250,"locked_reason":null},"extra_usage":{"is_enabled":false,"monthly_limit":3750,"used_credits":251,"utilization":6.69,"currency":"GBP","decimal_places":2,"disabled_reason":"out_of_credits","user_disabled":false,"spend_limit_reached":false,"credits_ever_enabled":true,"daily":null,"weekly":null}},"limits":[{"kind":"session","group":"session","percent":11,"severity":"normal","resets_at":"2026-09-27T19:59:59.718097+00:00","scope":null,"is_active":true},{"kind":"weekly_all","group":"weekly","percent":3,"severity":"normal","resets_at":"2026-10-04T14:59:59.718121+00:00","scope":null,"is_active":false},{"kind":"weekly_scoped","group":"weekly","percent":0,"severity":"normal","resets_at":"2026-10-04T15:00:00+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}],"spend":{"used":{"amount_minor":251,"currency":"GBP","exponent":2},"limit":{"amount_minor":3750,"currency":"GBP","exponent":2},"percent":7,"severity":"normal","enabled":false,"disabled_reason":"out_of_credits","cap":{"money":{"amount_minor":3750,"currency":"GBP","exponent":2},"credits":null},"balance":null,"auto_reload":null,"can_purchase_credits":false,"can_toggle":false}}`
)

type wantWindow struct {
	id    string
	kind  harness.QuotaKind
	model string
	used  float64
}

func requireWindows(t *testing.T, q harness.QuotaSnapshot, want []wantWindow) {
	t.Helper()
	if len(q.Windows) != len(want) {
		t.Fatalf("windows: got %+v, want %+v", q.Windows, want)
	}
	for i, w := range want {
		got := q.Windows[i]
		if got.ID != w.id || got.Kind != w.kind || got.Model != w.model || got.UsedPercent == nil || *got.UsedPercent != w.used {
			t.Fatalf("window %d: got %+v, want %+v", i, got, w)
		}
	}
}

func TestCodexVerifiedRateLimits(t *testing.T) {
	q, err := parseCodexQuota(json.RawMessage(codexRateLimitsFixture))
	if err != nil {
		t.Fatal(err)
	}
	requireWindows(t, q, []wantWindow{{"codex/primary", harness.QuotaWeekly, "", 2}})
	if q.LimitReached == nil || *q.LimitReached || !q.Windows[0].ResetsAt.Equal(time.Unix(1791113081, 0)) {
		t.Fatalf("quota state lost: %+v", q)
	}
	c, err := parseCodexCredits(json.RawMessage(codexRateLimitsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Known() || c.Enabled == nil || *c.Enabled || c.Unlimited == nil || *c.Unlimited || c.LimitReached == nil || *c.LimitReached {
		t.Fatalf("credit flags lost: %+v", c)
	}
	if c.Balance == nil || *c.Balance != (harness.Amount{Value: "0", Unit: harness.CreditUnit}) || c.ResetsAvailable == nil || *c.ResetsAvailable != 2 {
		t.Fatalf("credit amounts lost: %+v", c)
	}
	if c.Used != nil || c.Limit != nil || c.DisabledReason != "" {
		t.Fatalf("credits invented: %+v", c)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "fixture description") || strings.Contains(string(raw), "fixture-account") {
		t.Fatal("provider text escaped")
	}
}

func TestCodexQuotaKinds(t *testing.T) {
	q, err := parseCodexQuota(json.RawMessage(`{"rateLimitsByLimitId":{"a":{"primary":{"usedPercent":1,"windowDurationMins":300},"secondary":{"usedPercent":2,"windowDurationMins":10080}},"b":{"normalModelSlug":"fixture-model","primary":{"usedPercent":3,"windowDurationMins":10080},"secondary":{"usedPercent":4,"windowDurationMins":60}},"c":{"primary":{"usedPercent":5}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	requireWindows(t, q, []wantWindow{
		{"a/primary", harness.QuotaSession, "", 1},
		{"a/secondary", harness.QuotaWeekly, "", 2},
		{"b/primary", harness.QuotaWeeklyModel, "fixture-model", 3},
		{"b/secondary", harness.QuotaOther, "fixture-model", 4},
		{"c/primary", harness.QuotaOther, "", 5},
	})
	if q.LimitReached != nil {
		t.Fatal("limit state invented")
	}
}

func TestCodexLimitReached(t *testing.T) {
	for raw, want := range map[string]*bool{
		`{"ordinaryUsageAllowed":false,"rateLimits":{"primary":{"usedPercent":100}}}`:                                  ptr(true),
		`{"ordinaryUsageAllowed":null,"rateLimits":{"rateLimitReachedType":"rate_limit_reached"}}`:                     ptr(true),
		`{"rateLimits":{"rateLimitReachedType":null,"primary":{"usedPercent":100}}}`:                                   nil,
		`{"ordinaryUsageAllowed":true,"rateLimits":{"rateLimitReachedType":"workspace_owner_credits_depleted"}}`:       ptr(false),
		`{"rateLimits":{"limitId":"codex","rateLimitReachedType":"rate_limit_reached","primary":{"usedPercent":100}}}`: ptr(true),
	} {
		q, err := parseCodexQuota(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if (q.LimitReached == nil) != (want == nil) || (want != nil && *q.LimitReached != *want) {
			t.Fatalf("%s: limit reached %v, want %v", raw, q.LimitReached, want)
		}
	}
}

func TestCodexCreditsUnknownAndMalformed(t *testing.T) {
	c, err := parseCodexCredits(json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":1}}}`))
	if err != nil || c.Known() || c.Reason == "" || c.Enabled != nil || c.Balance != nil {
		t.Fatalf("absent credits became known: %+v %v", c, err)
	}
	c, err = parseCodexCredits(json.RawMessage(`{"rateLimits":{"credits":{"hasCredits":true,"unlimited":true,"balance":null}}}`))
	if err != nil || !c.Known() || c.Balance != nil || !*c.Unlimited {
		t.Fatalf("unlimited credits: %+v %v", c, err)
	}
	for _, raw := range []string{
		`{"rateLimits":{"credits":{"unlimited":false,"balance":"1"}}}`,
		`{"rateLimits":{"credits":{"hasCredits":true,"unlimited":false,"balance":"lots"}}}`,
		`{"rateLimits":{},"rateLimitResetCredits":{"availableCount":-1}}`,
		`{}`,
		`[]`,
	} {
		if _, err := parseCodexCredits(json.RawMessage(raw)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted malformed credits %s: %v", raw, err)
		}
	}
}

func TestClaudeVerifiedUsage(t *testing.T) {
	q, err := parseClaudeQuota(json.RawMessage(claudeUsageFixture))
	if err != nil {
		t.Fatal(err)
	}
	requireWindows(t, q, []wantWindow{
		{"five_hour", harness.QuotaSession, "", 11},
		{"iguana_necktie", harness.QuotaOther, "", 0},
		{"seven_day", harness.QuotaWeekly, "", 3},
		{"limits/weekly_model/Fable", harness.QuotaWeeklyModel, "Fable", 0},
	})
	if *q.Windows[0].WindowMinutes != 300 || *q.Windows[2].WindowMinutes != 10080 || *q.Windows[3].WindowMinutes != 10080 || q.Windows[1].WindowMinutes != nil {
		t.Fatalf("window durations: %+v", q.Windows)
	}
	if a := q.Windows[1].Allowance; a == nil || a.Limit != 250 || *a.Used != 0 {
		t.Fatalf("allowance lost: %+v", q.Windows[1])
	}
	if !q.Windows[3].ResetsAt.Equal(time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)) || q.LimitReached != nil {
		t.Fatalf("scoped window: %+v", q)
	}
	c, err := parseClaudeCredits(json.RawMessage(claudeUsageFixture))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Known() || c.Enabled == nil || *c.Enabled || c.DisabledReason != "out_of_credits" || c.LimitReached == nil || *c.LimitReached {
		t.Fatalf("credit state lost: %+v", c)
	}
	if *c.Used != (harness.Amount{Value: "2.51", Unit: "GBP"}) || *c.Limit != (harness.Amount{Value: "37.50", Unit: "GBP"}) {
		t.Fatalf("credit amounts: %+v %+v", c.Used, c.Limit)
	}
	if c.Balance != nil || c.Unlimited != nil || c.ResetsAvailable != nil {
		t.Fatalf("credits invented: %+v", c)
	}
}

func TestClaudeExtraUsageFallback(t *testing.T) {
	c, err := parseClaudeCredits(json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"extra_usage":{"is_enabled":true,"monthly_limit":null,"used_credits":5,"currency":"USD","decimal_places":2,"disabled_reason":null,"spend_limit_reached":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !*c.Enabled || !*c.LimitReached || *c.Used != (harness.Amount{Value: "0.05", Unit: "USD"}) || c.Limit != nil || c.DisabledReason != "" {
		t.Fatalf("extra usage fallback: %+v", c)
	}
	c, err = parseClaudeCredits(json.RawMessage(`{"rate_limits_available":false,"rate_limits":null}`))
	if err != nil || c.Known() || c.Reason == "" {
		t.Fatalf("absent credits became known: %+v %v", c, err)
	}
	c, err = parseClaudeCredits(json.RawMessage(`{"rate_limits_available":true,"spend":{"enabled":true,"disabled_reason":"Your card was declined, call 555","balance":{"amount_minor":-150,"currency":"EUR","exponent":2}}}`))
	if err != nil || c.DisabledReason != "" || *c.Balance != (harness.Amount{Value: "-1.50", Unit: "EUR"}) {
		t.Fatalf("free text kept or balance lost: %+v %v", c, err)
	}
	for _, raw := range []string{
		`{"spend":{"used":{"amount_minor":251,"currency":"gbp","exponent":2}}}`,
		`{"spend":{"used":{"amount_minor":251,"currency":"GBP"}}}`,
		`{"spend":{"used":{"amount_minor":2.5,"currency":"GBP","exponent":2}}}`,
		`{"spend":{"limit":{"amount_minor":-1,"currency":"GBP","exponent":2}}}`,
		`{"rate_limits":{"extra_usage":{"used_credits":1,"currency":"GBP"}}}`,
		`{"spend":[]}`,
	} {
		if _, err := parseClaudeCredits(json.RawMessage(raw)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted malformed credits %s: %v", raw, err)
		}
	}
}

func TestClaudeNamedLimitsDescribeExistingWindows(t *testing.T) {
	q, err := parseClaudeQuota(json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"seven_day_opus":{"utilization":40,"resets_at":"2030-01-01T00:00:00Z"},"monthly_pool":{"utilization":7,"resets_at":"2030-02-01T00:00:00Z"}},"limits":[{"kind":"weekly_scoped","group":"weekly","percent":40,"resets_at":"2030-01-01T00:00:00Z","scope":{"model":{"id":"claude-opus","display_name":"Opus"}}},{"kind":"monthly_all","group":"monthly","percent":7,"resets_at":"2030-02-01T00:00:00Z"},{"kind":"future","group":"future","percent":null,"resets_at":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	requireWindows(t, q, []wantWindow{
		{"monthly_pool", harness.QuotaMonthly, "", 7},
		{"seven_day_opus", harness.QuotaWeeklyModel, "Opus", 40},
	})
	for _, raw := range []string{
		`{"rate_limits_available":true,"limits":[{"kind":"session","percent":-1}]}`,
		`{"rate_limits_available":true,"limits":[{"kind":"session","resets_at":"soon"}]}`,
		`{"rate_limits_available":true,"limits":{}}`,
	} {
		if _, err := parseClaudeQuota(json.RawMessage(raw)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted malformed limits %s: %v", raw, err)
		}
	}
}

func TestClaudeStreamLimitReached(t *testing.T) {
	q, err := parseClaudeQuotaEvent(json.RawMessage(`{"status":"rejected","rateLimitType":"seven_day_opus","utilization":1}`))
	if err != nil || q.LimitReached == nil || !*q.LimitReached {
		t.Fatalf("rejection lost: %+v %v", q, err)
	}
	requireWindows(t, q, []wantWindow{{"seven_day_opus", harness.QuotaWeeklyModel, "opus", 100}})
	q, _ = parseClaudeQuotaEvent(json.RawMessage(`{"status":"allowed_warning","rateLimitType":"five_hour","utilization":0.9}`))
	if *q.LimitReached || q.Windows[0].Kind != harness.QuotaSession {
		t.Fatalf("warning mistaken for limit: %+v", q)
	}
}

func TestMinorUnits(t *testing.T) {
	for _, tc := range []struct {
		minor    string
		exponent int
		want     string
	}{{"251", 2, "2.51"}, {"3750", 2, "37.50"}, {"5", 2, "0.05"}, {"0", 2, "0.00"}, {"-5", 2, "-0.05"}, {"7", 0, "7"}, {"1234", 3, "1.234"}} {
		if got, ok := minorUnits(tc.minor, tc.exponent); !ok || got != tc.want {
			t.Fatalf("%s e%d: %q", tc.minor, tc.exponent, got)
		}
	}
	for _, bad := range [][2]any{{"1.5", 2}, {"1", -1}, {"1", 19}, {"", 0}} {
		if _, ok := minorUnits(bad[0].(string), bad[1].(int)); ok {
			t.Fatalf("accepted %v", bad)
		}
	}
	for s, want := range map[string]bool{"0": true, "12.50": true, "-3": true, "1.": false, ".5": false, "1e3": false, "": false} {
		if decimal(s) != want {
			t.Fatalf("decimal(%q)", s)
		}
	}
}

func TestReadQuotaRefreshesCredits(t *testing.T) {
	s, w := fakeSession(t, harness.Claude)
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) { return json.RawMessage(claudeUsageFixture), nil }
	if _, err := s.ReadQuota(testContext(t)); err != nil {
		t.Fatal(err)
	}
	c := s.Telemetry().Credits
	if !c.Known() || c.Used.Value != "2.51" {
		t.Fatalf("credits not refreshed: %+v", c)
	}
	c.Used.Value = "999"
	if s.Telemetry().Credits.Used.Value != "2.51" {
		t.Fatal("caller mutated session credits")
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":20,"resets_at":null}},"spend":{"used":{"amount_minor":"x"}}}`), nil
	}
	q, err := s.ReadQuota(testContext(t))
	if !errors.Is(err, ErrProtocol) || !q.Known() || *q.Windows[0].UsedPercent != 20 {
		t.Fatalf("quota lost to a credit failure: %+v %v", q, err)
	}
	if c := s.Telemetry().Credits; !c.Invalidated || c.Used.Value != "2.51" {
		t.Fatalf("failed credit refresh erased or freshened data: %+v", c)
	}
}

func TestCodexCreditNotificationAndAccountChange(t *testing.T) {
	s, _, turn := startedCodexTurn(t)
	notify(s, `{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300},"credits":{"hasCredits":true,"unlimited":false,"balance":"12.5"},"spendControlReached":false}}}`)
	if c := s.Telemetry().Credits; !c.Known() || *c.Balance != (harness.Amount{Value: "12.5", Unit: harness.CreditUnit}) || c.Source != "account/rateLimits/updated" {
		t.Fatalf("notified credits lost: %+v", c)
	}
	if q := s.Telemetry().Quota; q.Windows[0].Kind != harness.QuotaSession {
		t.Fatalf("notified window kind: %+v", q)
	}
	var credits *harness.CreditSnapshot
	for credits == nil {
		select {
		case e := <-turn.Events():
			credits = e.Credits
		case <-testContext(t).Done():
			t.Fatal("no credits event")
		}
	}
	credits.Balance.Value = "0"
	if s.Telemetry().Credits.Balance.Value != "12.5" {
		t.Fatal("event shares session credits")
	}
	notify(s, `{"method":"account/updated","params":{"authMode":null,"planType":null}}`)
	if c := s.Telemetry().Credits; c.Known() || c.Balance != nil {
		t.Fatal("account change retained prior credits")
	}
}

func ptr[T any](v T) *T { return &v }
