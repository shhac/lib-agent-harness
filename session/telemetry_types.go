package session

import harness "github.com/shhac/lib-agent-harness"

// ContextSnapshot is current conversation occupancy, never accumulated billing
// usage. CapacityTokens is the provider's effective window; ModelCapacityTokens
// is the raw model window when separately reported. Optional fields remain nil
// until observed. Local estimates and post-compaction invalidation are explicit.
type ContextSnapshot struct {
	harness.Observation
	Model               string   `json:"model,omitempty"`
	UsedTokens          *int64   `json:"used_tokens,omitempty"`
	CapacityTokens      *int64   `json:"capacity_tokens,omitempty"`
	ModelCapacityTokens *int64   `json:"model_capacity_tokens,omitempty"`
	UsedPercent         *float64 `json:"used_percent,omitempty"`
	AutoCompactAtTokens *int64   `json:"auto_compact_at_tokens,omitempty"`
}

// Telemetry is a session's latest observations. Credits come from the same
// native response as Quota and are refreshed with it.
type Telemetry struct {
	Account harness.AccountSnapshot `json:"account"`
	Quota   harness.QuotaSnapshot   `json:"quota"`
	Credits harness.CreditSnapshot  `json:"credits"`
	Context ContextSnapshot         `json:"context"`
}

// Inspection does not create a conversation or invoke a model. Options.Provider
// selects the engine, binary and native login home; session policies and
// prompts are not used.
type Inspection struct {
	Engine       harness.Engine          `json:"engine"`
	Account      harness.AccountSnapshot `json:"account"`
	Quota        harness.QuotaSnapshot   `json:"quota"`
	Credits      harness.CreditSnapshot  `json:"credits"`
	Capabilities Capabilities            `json:"capabilities"`
}
