package session

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

func parseCodexAccount(raw json.RawMessage) (harness.AccountSnapshot, error) {
	var r struct {
		Account      json.RawMessage `json:"account"`
		RequiresAuth *bool           `json:"requiresOpenaiAuth"`
	}
	if json.Unmarshal(raw, &r) != nil || r.RequiresAuth == nil {
		return harness.AccountSnapshot{}, ErrProtocol
	}
	a := harness.AccountSnapshot{Observation: observation("account/read", harness.Measured)}
	if len(r.Account) == 0 {
		a.Quality = ""
		a.Reason = "CLI did not report account metadata"
		return a, nil
	}
	logged := string(r.Account) != "null"
	a.LoggedIn = &logged
	if !logged {
		return a, nil
	}
	var account struct{ Type, Email, PlanType string }
	if json.Unmarshal(r.Account, &account) != nil || account.Type == "" {
		return harness.AccountSnapshot{}, ErrProtocol
	}
	a.AuthMethod = account.Type
	a.Email = account.Email
	a.Plan = account.PlanType
	return a, nil
}

type codexQuotaBucket struct {
	ID        string            `json:"limitId"`
	Name      string            `json:"limitName"`
	Model     string            `json:"normalModelSlug"`
	Primary   *codexQuotaWindow `json:"primary"`
	Secondary *codexQuotaWindow `json:"secondary"`
	// Reached names the limit that stopped ordinary use; null says none has.
	Reached      json.RawMessage `json:"rateLimitReachedType"`
	Credits      *codexCredits   `json:"credits"`
	SpendReached *bool           `json:"spendControlReached"`
}
type codexQuotaWindow struct {
	Used    *float64 `json:"usedPercent"`
	Minutes *int64   `json:"windowDurationMins"`
	Reset   *int64   `json:"resetsAt"`
}
type codexCredits struct {
	HasCredits *bool   `json:"hasCredits"`
	Unlimited  *bool   `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// codexRateLimits is the account/rateLimits/read result, and the
// account/rateLimits/updated notification's parameters, which carry only
// rateLimits.
type codexRateLimits struct {
	Legacy  *codexQuotaBucket           `json:"rateLimits"`
	Buckets map[string]codexQuotaBucket `json:"rateLimitsByLimitId"`
	// Allowed is the backend's own judgement of whether included usage may
	// continue; the protocol forbids inferring it from percentages.
	Allowed *bool `json:"ordinaryUsageAllowed"`
	Resets  *struct {
		Available *int64 `json:"availableCount"`
	} `json:"rateLimitResetCredits"`
}

func parseCodexQuota(raw json.RawMessage) (harness.QuotaSnapshot, error) {
	var r codexRateLimits
	if json.Unmarshal(raw, &r) != nil {
		return harness.QuotaSnapshot{}, ErrProtocol
	}
	if r.Buckets == nil && r.Legacy == nil {
		return harness.QuotaSnapshot{}, ErrProtocol
	}
	q := harness.QuotaSnapshot{Observation: observation("account/rateLimits/read", harness.Measured), Complete: true, LimitReached: codexLimitReached(r), LimitReason: codexLimitReason(r)}
	if len(r.Buckets) == 0 && r.Legacy != nil {
		id := r.Legacy.ID
		if id == "" {
			id = "default"
		}
		r.Buckets = map[string]codexQuotaBucket{id: *r.Legacy}
	}
	ids := make([]string, 0, len(r.Buckets))
	for id := range r.Buckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := r.Buckets[id]
		for i, w := range []*codexQuotaWindow{b.Primary, b.Secondary} {
			if w == nil {
				continue
			}
			if !nonnegative(w.Used) || (w.Minutes != nil && *w.Minutes <= 0) || (w.Reset != nil && *w.Reset < 0) {
				return harness.QuotaSnapshot{}, ErrProtocol
			}
			name := "primary"
			if i == 1 {
				name = "secondary"
			}
			n := harness.QuotaWindow{Observation: q.Observation, ID: id + "/" + name, Kind: codexQuotaKind(w.Minutes, b.Model), Model: b.Model, Scope: id, Name: b.Name, UsedPercent: w.Used, WindowMinutes: w.Minutes}
			if b.Model != "" {
				n.Scope = b.Model
			}
			if w.Reset != nil {
				reset := time.Unix(*w.Reset, 0).UTC()
				n.ResetsAt = &reset
			}
			q.Windows = append(q.Windows, n)
		}
	}
	if len(q.Windows) == 0 {
		q.Quality = ""
		q.Reason = "CLI reported no allowance windows"
	}
	return q, nil
}

// codexQuotaKind names a window by its duration, the one thing Codex states
// about its period. A bucket for one model makes its weekly window per-model.
func codexQuotaKind(minutes *int64, model string) harness.QuotaKind {
	switch {
	case minutes == nil:
		return harness.QuotaOther
	case *minutes == 300:
		return harness.QuotaSession
	case *minutes == 10080 && model != "":
		return harness.QuotaWeeklyModel
	case *minutes == 10080:
		return harness.QuotaWeekly
	}
	return harness.QuotaOther
}

// codexLimitReached prefers the backend's ordinary-usage judgement, and falls
// back to the single-bucket view's named limit, which a notification carries.
func codexLimitReached(r codexRateLimits) *bool {
	if r.Allowed != nil {
		reached := !*r.Allowed
		return &reached
	}
	if r.Legacy == nil || len(r.Legacy.Reached) == 0 || string(r.Legacy.Reached) == "null" {
		return nil
	}
	reached := true
	return &reached
}

// codexLimitReason is the single-bucket view's named limit, such as
// workspace_owner_credits_depleted, which tells waiting from buying credits.
func codexLimitReason(r codexRateLimits) string {
	if r.Legacy == nil {
		return ""
	}
	var reason *string
	if json.Unmarshal(r.Legacy.Reached, &reason) != nil {
		return ""
	}
	return providerCode(reason)
}

// parseCodexCredits reads the credit balance, spend control and early-reset
// grants from the same response as the quota windows. The single-bucket view
// mirrors Codex's own account, so it is preferred over other buckets.
func parseCodexCredits(raw json.RawMessage) (harness.CreditSnapshot, error) {
	var r codexRateLimits
	if json.Unmarshal(raw, &r) != nil || (r.Buckets == nil && r.Legacy == nil) {
		return harness.CreditSnapshot{}, ErrProtocol
	}
	buckets := make([]codexQuotaBucket, 0, len(r.Buckets)+1)
	if r.Legacy != nil {
		buckets = append(buckets, *r.Legacy)
	}
	ids := make([]string, 0, len(r.Buckets))
	for id := range r.Buckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		buckets = append(buckets, r.Buckets[id])
	}
	c := harness.CreditSnapshot{Observation: observation("account/rateLimits/read", harness.Measured)}
	known := false
	for _, b := range buckets {
		if c.LimitReached == nil && b.SpendReached != nil {
			c.LimitReached = cloneValue(b.SpendReached)
			known = true
		}
		if b.Credits == nil || c.Enabled != nil {
			continue
		}
		if b.Credits.HasCredits == nil || b.Credits.Unlimited == nil {
			return harness.CreditSnapshot{}, ErrProtocol
		}
		c.Enabled, c.Unlimited = cloneValue(b.Credits.HasCredits), cloneValue(b.Credits.Unlimited)
		if b.Credits.Balance != nil {
			if !decimal(*b.Credits.Balance) {
				return harness.CreditSnapshot{}, ErrProtocol
			}
			c.Balance = &harness.Amount{Value: *b.Credits.Balance, Unit: harness.CreditUnit}
		}
		known = true
	}
	if r.Resets != nil && r.Resets.Available != nil {
		if *r.Resets.Available < 0 || *r.Resets.Available > math.MaxInt32 {
			return harness.CreditSnapshot{}, ErrProtocol
		}
		n := int(*r.Resets.Available)
		c.ResetsAvailable = &n
		known = true
	}
	if !known {
		c.Quality = ""
		c.Reason = "CLI reported no credit balance"
	}
	return c, nil
}

func parseCodexContext(raw json.RawMessage) (ContextSnapshot, error) {
	var r struct {
		Last *struct {
			Total *int64 `json:"totalTokens"`
		} `json:"last"`
		Capacity *int64 `json:"modelContextWindow"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Last == nil || r.Last.Total == nil || *r.Last.Total < 0 || (r.Capacity != nil && *r.Capacity <= 0) {
		return ContextSnapshot{}, ErrProtocol
	}
	c := ContextSnapshot{Observation: observation("thread/tokenUsage/updated", harness.Measured), UsedTokens: r.Last.Total, CapacityTokens: r.Capacity}
	setContextPercent(&c)
	return c, nil
}
