package harness

import "time"

// Account vocabulary: who is logged in, and what their usage draws on.
//
// A harness's usage is paid for by up to three independent systems, and an
// application decides how to combine them:
//
//   - Quota: subscription allowance windows, such as five hours or a week,
//     reported as a percentage used. A percentage is never a token count.
//   - Cost: token spend valued at API rates (Cost, in usage.go), present only
//     when the harness reports it. It is a valuation, not a bill.
//   - Credits: a prepaid or overage balance that can pay for usage beyond a
//     quota window, or instead of one.
//
// Every part is present in an AccountReport and says whether it is known. An
// Observation with an empty Quality is unknown, and its Reason says why; an
// unknown part is never zero, unlimited or free.

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
	// Reason is a library-authored explanation, never provider text.
	Reason string `json:"reason,omitempty"`
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

// QuotaKind is a window's normalized period, so an application can show "the
// five-hour window" or "the weekly window" without learning each engine's
// names. The provider's own identifier stays in QuotaWindow.ID.
type QuotaKind string

const (
	// QuotaSession is a rolling short window, such as five hours.
	QuotaSession QuotaKind = "session"
	// QuotaWeekly is a weekly window over all models.
	QuotaWeekly QuotaKind = "weekly"
	// QuotaWeeklyModel is a weekly window for one model; QuotaWindow.Model
	// names it.
	QuotaWeeklyModel QuotaKind = "weekly_model"
	QuotaMonthly     QuotaKind = "monthly"
	// QuotaOther is any window whose period the library does not recognize.
	QuotaOther QuotaKind = "other"
)

type QuotaWindow struct {
	Observation
	ID   string    `json:"id"`
	Kind QuotaKind `json:"kind"`
	// Model is the provider's name for the one model a window meters, as a
	// QuotaWeeklyModel window always does; empty for windows over every model.
	Model         string     `json:"model,omitempty"`
	Scope         string     `json:"scope,omitempty"`
	Name          string     `json:"name,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	WindowMinutes *int64     `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	Allowance     *Allowance `json:"allowance,omitempty"`
	// LimitReached is the provider's statement that this window is the one
	// refusing use; nil when it said nothing about this window. A limit on one
	// model's window leaves the others usable.
	LimitReached *bool `json:"limit_reached,omitempty"`
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
	// LimitReached is the provider's own statement that a subscription limit
	// currently blocks ordinary use. Nil means it did not say; it is never
	// inferred from percentages or reset times.
	LimitReached *bool `json:"limit_reached,omitempty"`
	// LimitReason is the provider's enumerated reason a limit was reached,
	// such as Codex's workspace_owner_credits_depleted, which says the remedy
	// is buying credits rather than waiting; empty when it did not say.
	LimitReason string `json:"limit_reason,omitempty"`
}

// Amount is an exact decimal as the provider reported it, such as "37.50", in
// an ISO 4217 currency or "credits". It is a string so that no value is ever
// rounded through a float.
type Amount struct {
	Value string `json:"value"`
	Unit  string `json:"unit"`
}

// CreditUnit is the Amount unit for a provider's own credits, which are not
// money.
const CreditUnit = "credits"

// CreditSnapshot is a prepaid or overage balance that can pay for usage
// beyond, or instead of, quota windows. Nil fields are unknown: a nil Balance
// is not an empty balance, and a nil Unlimited is not a limit.
type CreditSnapshot struct {
	Observation
	// Enabled says credits can currently pay for usage.
	Enabled   *bool `json:"enabled,omitempty"`
	Unlimited *bool `json:"unlimited,omitempty"`
	// LimitReached says a spend limit on credits has been reached.
	LimitReached *bool   `json:"limit_reached,omitempty"`
	Balance      *Amount `json:"balance,omitempty"`
	Used         *Amount `json:"used,omitempty"`
	Limit        *Amount `json:"limit,omitempty"`
	// DisabledReason is the provider's enumerated code, such as
	// "out_of_credits"; codes outside a plain identifier shape are dropped.
	DisabledReason string `json:"disabled_reason,omitempty"`
	// ResetsAvailable counts grants the account holds that reset its quota
	// windows early.
	ResetsAvailable *int `json:"resets_available,omitempty"`
}

// AccountReport is one engine's account, quota and credits. Every part is
// present; each part's Observation says whether it is known, and an unknown
// part's Reason says why, including when the engine cannot report it at all.
// Account may contain personal information (see AccountSnapshot).
type AccountReport struct {
	Engine  Engine          `json:"engine"`
	Account AccountSnapshot `json:"account"`
	Quota   QuotaSnapshot   `json:"quota"`
	Credits CreditSnapshot  `json:"credits"`
}
