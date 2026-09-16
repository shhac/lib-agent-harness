package session

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

func parseClaudeAccount(raw json.RawMessage) (AccountSnapshot, error) {
	var r struct {
		Account *struct{ Email, Organization, SubscriptionType, APIProvider string } `json:"account"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Account == nil {
		return AccountSnapshot{}, ErrProtocol
	}
	a := AccountSnapshot{Observation: observation("initialize.account", Measured), Email: r.Account.Email, Organization: r.Account.Organization, Plan: r.Account.SubscriptionType, Provider: r.Account.APIProvider}
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

func parseClaudeQuota(raw json.RawMessage) (QuotaSnapshot, error) {
	var r struct {
		Available *bool                      `json:"rate_limits_available"`
		Limits    map[string]json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Available == nil {
		return QuotaSnapshot{}, ErrProtocol
	}
	q := QuotaSnapshot{Observation: observation("get_usage", Measured), Complete: true}
	if !*r.Available || r.Limits == nil {
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
			return QuotaSnapshot{}, ErrProtocol
		}
		n := QuotaWindow{Observation: q.Observation, ID: id, Scope: id, UsedPercent: w.Utilization, WindowMinutes: claudeWindowMinutes(id)}
		if w.ResetsAt != nil && *w.ResetsAt != "" {
			when, err := time.Parse(time.RFC3339, *w.ResetsAt)
			if err != nil {
				return QuotaSnapshot{}, ErrProtocol
			}
			when = when.UTC()
			n.ResetsAt = &when
		}
		if w.Limit != nil {
			n.Allowance = &Allowance{Unit: "USD", Limit: *w.Limit, Used: w.Used}
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
			return QuotaSnapshot{}, ErrProtocol
		}
		for _, m := range models {
			if m.Name == "" || (m.Utilization != nil && !nonnegative(m.Utilization)) {
				return QuotaSnapshot{}, ErrProtocol
			}
			n := QuotaWindow{Observation: q.Observation, ID: "model:" + m.Name, Scope: m.Name, Name: m.Name, UsedPercent: m.Utilization, WindowMinutes: claudeWindowMinutes("seven_day")}
			if m.ResetsAt != nil && *m.ResetsAt != "" {
				when, err := time.Parse(time.RFC3339, *m.ResetsAt)
				if err != nil {
					return QuotaSnapshot{}, ErrProtocol
				}
				when = when.UTC()
				n.ResetsAt = &when
			}
			if n.UsedPercent != nil || n.ResetsAt != nil {
				q.Windows = append(q.Windows, n)
			}
		}
	}
	if len(q.Windows) == 0 {
		q.Quality = ""
		q.Reason = "CLI reported no allowance windows"
	}
	return q, nil
}

// Native stream utilization is a fraction; get_usage utilization is a percent.
// Keep the parsers separate so a legitimate 0.5 percent is never multiplied.
func parseClaudeQuotaEvent(raw json.RawMessage) (QuotaSnapshot, error) {
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
		return QuotaSnapshot{}, ErrProtocol
	}
	switch r.Status {
	case "allowed", "allowed_warning", "rejected":
	default:
		return QuotaSnapshot{}, ErrProtocol
	}
	q := QuotaSnapshot{Observation: observation("rate_limit_event", Measured), Status: r.Status, UsingOverage: r.UsingOverage}
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
			return QuotaSnapshot{}, ErrProtocol
		}
		n := QuotaWindow{Observation: q.Observation, ID: id, Scope: id, WindowMinutes: claudeWindowMinutes(id)}
		if w.Utilization != nil {
			pct := *w.Utilization * 100
			if !nonnegative(&pct) {
				return QuotaSnapshot{}, ErrProtocol
			}
			n.UsedPercent = &pct
		}
		if w.Reset != nil {
			when := time.Unix(*w.Reset, 0).UTC()
			n.ResetsAt = &when
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
	c := ContextSnapshot{Observation: observation("get_context_usage.summary", Estimated), Model: r.Model, UsedTokens: r.Total, CapacityTokens: r.Max, ModelCapacityTokens: r.RawMax, AutoCompactAtTokens: r.Threshold}
	setContextPercent(&c)
	return c, nil
}
func claudeWindowMinutes(id string) *int64 {
	var n int64
	switch {
	case id == "five_hour":
		n = 300
	case id == "seven_day" || strings.HasPrefix(id, "seven_day_"):
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
