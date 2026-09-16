package session

import (
	"encoding/json"
	"sort"
	"time"
)

func parseCodexAccount(raw json.RawMessage) (AccountSnapshot, error) {
	var r struct {
		Account      json.RawMessage `json:"account"`
		RequiresAuth *bool           `json:"requiresOpenaiAuth"`
	}
	if json.Unmarshal(raw, &r) != nil || r.RequiresAuth == nil {
		return AccountSnapshot{}, ErrProtocol
	}
	a := AccountSnapshot{Observation: observation("account/read", Measured)}
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
		return AccountSnapshot{}, ErrProtocol
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
}
type codexQuotaWindow struct {
	Used    *float64 `json:"usedPercent"`
	Minutes *int64   `json:"windowDurationMins"`
	Reset   *int64   `json:"resetsAt"`
}

func parseCodexQuota(raw json.RawMessage) (QuotaSnapshot, error) {
	var r struct {
		Legacy  *codexQuotaBucket           `json:"rateLimits"`
		Buckets map[string]codexQuotaBucket `json:"rateLimitsByLimitId"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return QuotaSnapshot{}, ErrProtocol
	}
	if r.Buckets == nil && r.Legacy == nil {
		return QuotaSnapshot{}, ErrProtocol
	}
	q := QuotaSnapshot{Observation: observation("account/rateLimits/read", Measured), Complete: true}
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
				return QuotaSnapshot{}, ErrProtocol
			}
			name := "primary"
			if i == 1 {
				name = "secondary"
			}
			n := QuotaWindow{Observation: q.Observation, ID: id + "/" + name, Scope: id, Name: b.Name, UsedPercent: w.Used, WindowMinutes: w.Minutes}
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
	c := ContextSnapshot{Observation: observation("thread/tokenUsage/updated", Measured), UsedTokens: r.Last.Total, CapacityTokens: r.Capacity}
	setContextPercent(&c)
	return c, nil
}
