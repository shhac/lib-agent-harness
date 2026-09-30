package session

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/claudeproto"
)

func parseClaudeAccount(raw json.RawMessage) (harness.AccountSnapshot, error) {
	var r struct {
		Account *struct{ Email, Organization, SubscriptionType, APIProvider string } `json:"account"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Account == nil {
		return harness.AccountSnapshot{}, ErrProtocol
	}
	a := harness.AccountSnapshot{Observation: observation("initialize.account", harness.Measured), Email: r.Account.Email, Organization: r.Account.Organization, Plan: r.Account.SubscriptionType, Provider: r.Account.APIProvider}
	if a.Email == "" && a.Plan == "" && a.Organization == "" && a.Provider == "" {
		a.Quality = ""
		a.Reason = "CLI did not report account metadata"
	}
	// The handshake has no explicit loggedIn boolean. Provider selection alone
	// does not establish successful authentication.
	if a.Email != "" || a.Plan != "" {
		v := true
		a.LoggedIn = &v
	}
	return a, nil
}

type claudeQuotaWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
	Limit       *float64 `json:"limit_dollars"`
	Used        *float64 `json:"used_dollars"`
}

// claudeLimit is one entry of get_usage's limits array, which names each
// window's kind, group and model scope directly.
type claudeLimit struct {
	Kind     string   `json:"kind"`
	Group    string   `json:"group"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			ID   *string `json:"id"`
			Name *string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func parseClaudeQuota(raw json.RawMessage) (harness.QuotaSnapshot, error) {
	var r struct {
		Available *bool                      `json:"rate_limits_available"`
		Limits    map[string]json.RawMessage `json:"rate_limits"`
		Named     []claudeLimit              `json:"limits"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Available == nil {
		return harness.QuotaSnapshot{}, ErrProtocol
	}
	q := harness.QuotaSnapshot{Observation: observation("get_usage", harness.Measured), Complete: true}
	if !*r.Available || (r.Limits == nil && r.Named == nil) {
		q.Quality = ""
		q.Reason = "CLI did not supply subscription allowances for this login"
		return q, nil
	}
	ids := make([]string, 0, len(r.Limits))
	for id := range r.Limits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		raw := r.Limits[id]
		if string(raw) == "null" {
			continue
		}
		// Only allowance windows have these fields. Ignore unrelated billing,
		// behavior summaries and aggregated views; never infer token caps from them.
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			continue
		}
		_, usage := fields["utilization"]
		_, reset := fields["resets_at"]
		if !usage || !reset {
			continue
		}
		var w claudeQuotaWindow
		if json.Unmarshal(raw, &w) != nil || (w.Utilization != nil && !nonnegative(w.Utilization)) || (w.Limit != nil && !nonnegative(w.Limit)) || (w.Used != nil && !nonnegative(w.Used)) {
			return harness.QuotaSnapshot{}, ErrProtocol
		}
		kind, model := claudeQuotaKind(id)
		n := harness.QuotaWindow{Observation: q.Observation, ID: id, Kind: kind, Model: model, Scope: id, UsedPercent: w.Utilization, WindowMinutes: claudeWindowMinutes(kind)}
		var err error
		if n.ResetsAt, err = claudeReset(w.ResetsAt); err != nil {
			return harness.QuotaSnapshot{}, err
		}
		if w.Limit != nil {
			n.Allowance = &harness.Allowance{Unit: "USD", Limit: *w.Limit, Used: w.Used}
		}
		if n.UsedPercent == nil && n.ResetsAt == nil && n.Allowance == nil {
			continue
		}
		q.Windows = append(q.Windows, n)
	}
	// Newer CLIs also expose server-named model buckets as an array. Do not
	// require a hardcoded model list to retain those weekly allowances.
	if raw, ok := r.Limits["model_scoped"]; ok && string(raw) != "null" {
		var models []struct {
			Name string `json:"display_name"`
			claudeQuotaWindow
		}
		if json.Unmarshal(raw, &models) != nil {
			return harness.QuotaSnapshot{}, ErrProtocol
		}
		for _, m := range models {
			if m.Name == "" || (m.Utilization != nil && !nonnegative(m.Utilization)) {
				return harness.QuotaSnapshot{}, ErrProtocol
			}
			n := harness.QuotaWindow{Observation: q.Observation, ID: "model:" + m.Name, Kind: harness.QuotaWeeklyModel, Model: m.Name, Scope: m.Name, Name: m.Name, UsedPercent: m.Utilization, WindowMinutes: claudeWindowMinutes(harness.QuotaWeeklyModel)}
			var err error
			if n.ResetsAt, err = claudeReset(m.ResetsAt); err != nil {
				return harness.QuotaSnapshot{}, err
			}
			if n.UsedPercent != nil || n.ResetsAt != nil {
				q.Windows = append(q.Windows, n)
			}
		}
	}
	windows, err := claudeNamedLimits(q.Windows, r.Named, q.Observation)
	if err != nil {
		return harness.QuotaSnapshot{}, err
	}
	q.Windows = windows
	if len(q.Windows) == 0 {
		q.Quality = ""
		q.Reason = "CLI reported no allowance windows"
	}
	return q, nil
}

// claudeNamedLimits applies the limits array, which says what each window is
// instead of leaving it to be read from a name. An entry describing a window
// already read from rate_limits (the same kind, or an unrecognized one, resetting
// at the same moment, or the same model) lends it its kind and model; any other
// entry is a window of its own.
func claudeNamedLimits(windows []harness.QuotaWindow, named []claudeLimit, o harness.Observation) ([]harness.QuotaWindow, error) {
	matched := make([]bool, len(windows))
	for _, l := range named {
		if l.Percent != nil && !nonnegative(l.Percent) {
			return nil, ErrProtocol
		}
		reset, err := claudeReset(l.ResetsAt)
		if err != nil {
			return nil, err
		}
		kind, model := claudeLimitKind(l)
		i := claudeLimitMatch(windows, matched, kind, model, reset)
		if i >= 0 {
			matched[i] = true
			windows[i].Kind, windows[i].WindowMinutes = kind, claudeWindowMinutes(kind)
			if model != "" {
				windows[i].Model = model
			}
			continue
		}
		if l.Percent == nil && reset == nil {
			continue
		}
		id := "limits/" + string(kind)
		if model != "" {
			id += "/" + model
		}
		n := harness.QuotaWindow{Observation: o, ID: id, Kind: kind, Model: model, Scope: l.Kind, Name: model, UsedPercent: cloneValue(l.Percent), WindowMinutes: claudeWindowMinutes(kind), ResetsAt: reset}
		windows = append(windows, n)
		matched = append(matched, true)
	}
	return windows, nil
}

func claudeLimitMatch(windows []harness.QuotaWindow, matched []bool, kind harness.QuotaKind, model string, reset *time.Time) int {
	for i, w := range windows {
		if matched[i] {
			continue
		}
		if kind == harness.QuotaWeeklyModel {
			if w.Kind == kind && model != "" && w.Model != "" && strings.Contains(strings.ToLower(model), strings.ToLower(w.Model)) {
				return i
			}
			continue
		}
		if (w.Kind == kind || w.Kind == harness.QuotaOther) && reset != nil && w.ResetsAt != nil && reset.Equal(*w.ResetsAt) {
			return i
		}
	}
	return -1
}

func claudeLimitKind(l claudeLimit) (harness.QuotaKind, string) {
	model := ""
	if l.Scope != nil && l.Scope.Model != nil {
		if l.Scope.Model.Name != nil {
			model = *l.Scope.Model.Name
		} else if l.Scope.Model.ID != nil {
			model = *l.Scope.Model.ID
		}
	}
	switch {
	case l.Kind == "session" || l.Group == "session":
		return harness.QuotaSession, model
	case l.Group == "weekly" && (model != "" || l.Kind == "weekly_scoped"):
		return harness.QuotaWeeklyModel, model
	case l.Group == "weekly":
		return harness.QuotaWeekly, model
	case l.Group == "monthly":
		return harness.QuotaMonthly, model
	}
	return harness.QuotaOther, model
}

// claudeQuotaKind reads a rate_limits key: five_hour, seven_day, and
// seven_day_<model> for a model's own weekly window.
func claudeQuotaKind(id string) (harness.QuotaKind, string) {
	switch {
	case id == "five_hour":
		return harness.QuotaSession, ""
	case id == "seven_day":
		return harness.QuotaWeekly, ""
	case strings.HasPrefix(id, "seven_day_") && len(id) > len("seven_day_"):
		return harness.QuotaWeeklyModel, strings.TrimPrefix(id, "seven_day_")
	}
	return harness.QuotaOther, ""
}

func claudeReset(value *string) (*time.Time, error) {
	if value == nil || *value == "" {
		return nil, nil
	}
	when, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, ErrProtocol
	}
	when = when.UTC()
	return &when, nil
}

// claudeMoney is an amount in a currency's minor units: 251 at exponent 2 is
// 2.51.
type claudeMoney struct {
	Minor    *json.Number `json:"amount_minor"`
	Currency string       `json:"currency"`
	Exponent *int         `json:"exponent"`
}

// parseClaudeCredits reads extra usage, Claude's overage that is paid for
// beyond the subscription windows. The spend view is preferred; the older
// extra_usage window fills whatever it does not state.
func parseClaudeCredits(raw json.RawMessage) (harness.CreditSnapshot, error) {
	var r struct {
		Spend *struct {
			Used           *claudeMoney    `json:"used"`
			Limit          *claudeMoney    `json:"limit"`
			Balance        json.RawMessage `json:"balance"`
			Enabled        *bool           `json:"enabled"`
			DisabledReason *string         `json:"disabled_reason"`
		} `json:"spend"`
		Limits *struct {
			Extra *struct {
				Enabled        *bool        `json:"is_enabled"`
				Limit          *json.Number `json:"monthly_limit"`
				Used           *json.Number `json:"used_credits"`
				Currency       string       `json:"currency"`
				Places         *int         `json:"decimal_places"`
				DisabledReason *string      `json:"disabled_reason"`
				Reached        *bool        `json:"spend_limit_reached"`
			} `json:"extra_usage"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return harness.CreditSnapshot{}, ErrProtocol
	}
	c := harness.CreditSnapshot{Observation: observation("get_usage", harness.Measured)}
	known := false
	if s := r.Spend; s != nil {
		var err error
		if c.Used, err = s.Used.amount(); err != nil {
			return harness.CreditSnapshot{}, err
		}
		if c.Limit, err = s.Limit.amount(); err != nil {
			return harness.CreditSnapshot{}, err
		}
		// The balance's shape has not been observed populated; an object that
		// is not money stays unknown rather than failing the whole report.
		var balance claudeMoney
		if len(s.Balance) > 0 && string(s.Balance) != "null" && json.Unmarshal(s.Balance, &balance) == nil && balance.Minor != nil {
			if c.Balance, err = balance.signedAmount(); err != nil {
				return harness.CreditSnapshot{}, err
			}
		}
		c.Enabled = cloneValue(s.Enabled)
		c.DisabledReason = providerCode(s.DisabledReason)
		known = true
	}
	if r.Limits != nil && r.Limits.Extra != nil {
		e := r.Limits.Extra
		if c.Enabled == nil {
			c.Enabled = cloneValue(e.Enabled)
		}
		if c.DisabledReason == "" {
			c.DisabledReason = providerCode(e.DisabledReason)
		}
		c.LimitReached = cloneValue(e.Reached)
		for _, f := range []struct {
			into  **harness.Amount
			minor *json.Number
		}{{&c.Limit, e.Limit}, {&c.Used, e.Used}} {
			if *f.into != nil || f.minor == nil {
				continue
			}
			amount, err := (&claudeMoney{Minor: f.minor, Currency: e.Currency, Exponent: e.Places}).amount()
			if err != nil {
				return harness.CreditSnapshot{}, err
			}
			*f.into = amount
		}
		known = true
	}
	if !known {
		c.Quality = ""
		c.Reason = "CLI reported no extra usage"
	}
	return c, nil
}

func (m *claudeMoney) amount() (*harness.Amount, error) {
	a, err := m.signedAmount()
	if err != nil || a == nil {
		return a, err
	}
	if strings.HasPrefix(a.Value, "-") {
		return nil, ErrProtocol
	}
	return a, nil
}

func (m *claudeMoney) signedAmount() (*harness.Amount, error) {
	if m == nil {
		return nil, nil
	}
	if m.Minor == nil || m.Exponent == nil || !currencyCode(m.Currency) {
		return nil, ErrProtocol
	}
	value, ok := minorUnits(string(*m.Minor), *m.Exponent)
	if !ok {
		return nil, ErrProtocol
	}
	return &harness.Amount{Value: value, Unit: m.Currency}, nil
}

// Native stream utilization is a fraction; get_usage utilization is a percent.
// Keep the parsers separate so a legitimate 0.5 percent is never multiplied.
func parseClaudeQuotaEvent(raw json.RawMessage) (harness.QuotaSnapshot, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		Reset       *int64   `json:"resetsAt"`
	}
	var r struct {
		Status string `json:"status"`
		Type   string `json:"rateLimitType"`
		window
		Windows      map[string]window `json:"unifiedWindows"`
		UsingOverage *bool             `json:"isUsingOverage"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return harness.QuotaSnapshot{}, ErrProtocol
	}
	switch r.Status {
	case "allowed", "allowed_warning", "rejected":
	default:
		return harness.QuotaSnapshot{}, ErrProtocol
	}
	reached := r.Status == "rejected"
	q := harness.QuotaSnapshot{Observation: observation("rate_limit_event", harness.Measured), Status: r.Status, UsingOverage: r.UsingOverage, LimitReached: &reached}
	if r.Windows == nil {
		r.Windows = map[string]window{}
	}
	if r.Type != "" {
		if _, ok := r.Windows[r.Type]; !ok {
			r.Windows[r.Type] = r.window
		}
	}
	ids := make([]string, 0, len(r.Windows))
	for id := range r.Windows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := r.Windows[id]
		if (w.Utilization != nil && !nonnegative(w.Utilization)) || (w.Reset != nil && *w.Reset < 0) {
			return harness.QuotaSnapshot{}, ErrProtocol
		}
		kind, model := claudeQuotaKind(id)
		n := harness.QuotaWindow{Observation: q.Observation, ID: id, Kind: kind, Model: model, Scope: id, WindowMinutes: claudeWindowMinutes(kind)}
		// A rejection names the window that refused; while use is allowed, no
		// window in the event is refusing it.
		switch {
		case r.Status == "rejected" && id == r.Type:
			n.LimitReached = cloneValue(&reached)
		case r.Status != "rejected":
			n.LimitReached = cloneValue(&reached)
		}
		if w.Utilization != nil {
			pct := *w.Utilization * 100
			if !nonnegative(&pct) {
				return harness.QuotaSnapshot{}, ErrProtocol
			}
			n.UsedPercent = &pct
		}
		if w.Reset != nil {
			n.ResetsAt = claudeproto.Reset(*w.Reset, time.Now())
		}
		q.Windows = append(q.Windows, n)
	}
	return q, nil
}

func parseClaudeContext(raw json.RawMessage) (ContextSnapshot, error) {
	var r struct {
		Total     *int64 `json:"totalTokens"`
		Max       *int64 `json:"maxTokens"`
		RawMax    *int64 `json:"rawMaxTokens"`
		Threshold *int64 `json:"autoCompactThreshold"`
		Model     string `json:"model"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Total == nil || *r.Total < 0 || r.Max == nil || *r.Max <= 0 || (r.RawMax != nil && *r.RawMax <= 0) || (r.Threshold != nil && *r.Threshold < 0) {
		return ContextSnapshot{}, ErrProtocol
	}
	c := ContextSnapshot{Observation: observation("get_context_usage.summary", harness.Estimated), Model: r.Model, UsedTokens: r.Total, CapacityTokens: r.Max, ModelCapacityTokens: r.RawMax, AutoCompactAtTokens: r.Threshold}
	setContextPercent(&c)
	return c, nil
}
func claudeWindowMinutes(kind harness.QuotaKind) *int64 {
	var n int64
	switch kind {
	case harness.QuotaSession:
		n = 300
	case harness.QuotaWeekly, harness.QuotaWeeklyModel:
		n = 10080
	default:
		return nil
	}
	return &n
}
func nonnegative(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0
}
func setContextPercent(c *ContextSnapshot) {
	c.UsedPercent = nil
	if c.UsedTokens != nil && c.CapacityTokens != nil && *c.CapacityTokens > 0 {
		v := float64(*c.UsedTokens) / float64(*c.CapacityTokens) * 100
		c.UsedPercent = &v
	}
}
