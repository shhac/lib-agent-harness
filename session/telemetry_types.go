package session

import "time"

// Measurement describes evidence, not capability. Empty means unavailable.
// Estimated values must not be presented as exact provider measurements.
type Measurement string

const (
	Measured  Measurement = "measured"
	Estimated Measurement = "estimated"
)

// Observation records when the CLI supplied a value. This is the observation
// time, not necessarily the upstream measurement time: a CLI may cache data.
// Callers choose their own freshness limit. Failed refreshes retain the previous
// value and timestamp, with Invalidated set, rather than reporting zero usage.
type Observation struct {
	Quality     Measurement `json:"quality,omitempty"`
	ObservedAt  time.Time   `json:"observed_at"`
	Source      string      `json:"source,omitempty"`
	Invalidated bool        `json:"invalidated,omitempty"`
	Reason      string      `json:"reason,omitempty"`
}

func (o Observation) Known() bool { return o.Quality == Measured || o.Quality == Estimated }

// IsStale also returns true for unknown or explicitly invalidated observations.
// maxAge <= 0 disables only the age check, not the invalidation check.
func (o Observation) IsStale(now time.Time, maxAge time.Duration) bool {
	return !o.Known() || o.Invalidated || o.ObservedAt.IsZero() || (maxAge > 0 && now.Sub(o.ObservedAt) > maxAge)
}

// AccountSnapshot contains non-secret login metadata reported by the CLI. It
// may contain personal information: do not publish it as anonymous telemetry.
// Nil LoggedIn and empty strings mean unknown, not logged out or a free plan.
type AccountSnapshot struct {
	Observation
	LoggedIn     *bool  `json:"logged_in,omitempty"`
	AuthMethod   string `json:"auth_method,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Email        string `json:"email,omitempty"`
	Organization string `json:"organization,omitempty"`
	Plan         string `json:"plan,omitempty"`
}

// Allowance is present only when the provider reports an absolute cap and its
// unit. Percentages must never be reverse-engineered into token/message caps.
// USD limits describe a provider allowance, not a purchase authorization.
type Allowance struct {
	Unit  string   `json:"unit"`
	Limit float64  `json:"limit"`
	Used  *float64 `json:"used,omitempty"`
}

type QuotaWindow struct {
	Observation
	ID            string     `json:"id"`
	Scope         string     `json:"scope,omitempty"`
	Name          string     `json:"name,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	WindowMinutes *int64     `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	Allowance     *Allowance `json:"allowance,omitempty"`
}

// RemainingPercent clamps only the derived remainder. UsedPercent preserves
// values above 100 when the provider reports usage beyond its ordinary cap.
func (w QuotaWindow) RemainingPercent() *float64 {
	if w.UsedPercent == nil {
		return nil
	}
	v := max(0, 100-*w.UsedPercent)
	return &v
}

type QuotaSnapshot struct {
	Observation
	// Complete means this was a snapshot, rather than an incremental event.
	// It does not promise the provider exposed every limit that applies.
	Complete     bool          `json:"complete"`
	Windows      []QuotaWindow `json:"windows"`
	Status       string        `json:"status,omitempty"`
	UsingOverage *bool         `json:"using_overage,omitempty"`
}

// ContextSnapshot is current conversation occupancy, never accumulated billing
// usage. CapacityTokens is the provider's effective window; ModelCapacityTokens
// is the raw model window when separately reported. Optional fields remain nil
// until observed. Local estimates and post-compaction invalidation are explicit.
type ContextSnapshot struct {
	Observation
	Model               string   `json:"model,omitempty"`
	UsedTokens          *int64   `json:"used_tokens,omitempty"`
	CapacityTokens      *int64   `json:"capacity_tokens,omitempty"`
	ModelCapacityTokens *int64   `json:"model_capacity_tokens,omitempty"`
	UsedPercent         *float64 `json:"used_percent,omitempty"`
	AutoCompactAtTokens *int64   `json:"auto_compact_at_tokens,omitempty"`
}

type Telemetry struct {
	Account AccountSnapshot `json:"account"`
	Quota   QuotaSnapshot   `json:"quota"`
	Context ContextSnapshot `json:"context"`
}

// Inspection does not create a conversation or invoke a model. Options select
// the binary and native login home; session policies and prompts are not used.
type Inspection struct {
	Engine       Engine          `json:"engine"`
	Account      AccountSnapshot `json:"account"`
	Quota        QuotaSnapshot   `json:"quota"`
	Capabilities Capabilities    `json:"capabilities"`
}
