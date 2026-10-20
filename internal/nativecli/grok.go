package nativecli

import (
	"encoding/json"

	harness "github.com/shhac/lib-agent-harness"
)

// GrokReducedTelemetry opts out of Grok's client telemetry and stops its
// Cursor/Claude/Codex compatibility scanners importing other harnesses' local
// configuration (including their MCP servers). External OpenTelemetry
// (GROK_EXTERNAL_OTEL) is left alone: it reports to the operator's own
// collector, not to xAI.
//
// Every launch of Grok (native runs, catalog discovery, account inspection)
// uses this one list.
var GrokReducedTelemetry = []string{
	"GROK_TELEMETRY_ENABLED=0",
	"GROK_TELEMETRY_MIXPANEL_ENABLED=0",
	"GROK_TELEMETRY_TRACE_UPLOAD=0",
	"GROK_FEEDBACK_ENABLED=0",
	"GROK_DISABLE_AUTOUPDATER=1",
	"GROK_MEMORY=0",
	"GROK_CURSOR_SKILLS_ENABLED=0",
	"GROK_CURSOR_RULES_ENABLED=0",
	"GROK_CURSOR_AGENTS_ENABLED=0",
	"GROK_CURSOR_MCPS_ENABLED=0",
	"GROK_CURSOR_HOOKS_ENABLED=0",
	"GROK_CLAUDE_SKILLS_ENABLED=0",
	"GROK_CLAUDE_RULES_ENABLED=0",
	"GROK_CLAUDE_AGENTS_ENABLED=0",
	"GROK_CLAUDE_MCPS_ENABLED=0",
	"GROK_CLAUDE_HOOKS_ENABLED=0",
	"GROK_CLAUDE_SESSIONS_ENABLED=0",
	"GROK_CURSOR_SESSIONS_ENABLED=0",
	"GROK_CODEX_SKILLS_ENABLED=0",
	"GROK_CODEX_RULES_ENABLED=0",
	"GROK_CODEX_AGENTS_ENABLED=0",
	"GROK_CODEX_MCPS_ENABLED=0",
	"GROK_CODEX_HOOKS_ENABLED=0",
	"GROK_CODEX_SESSIONS_ENABLED=0",
}

// GrokInitialize opens Grok's agent protocol: the request every short Grok
// conversation (model discovery, account inspection) starts with, framed as
// one line.
const GrokInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}` + "\n"

// GrokCostTicksPerUSD: Grok states cost exactly in ticks of 10^-10 USD.
const GrokCostTicksPerUSD = 1e10

// GrokSubscription reads a check_subscription answer into an account
// snapshot observed as observed, reporting false for one that is not an
// answer. A nil LoggedIn and empty fields stay unknown.
func GrokSubscription(raw json.RawMessage, observed harness.Observation) (harness.AccountSnapshot, bool) {
	var r struct {
		Authenticated *bool `json:"authenticated"`
		Meta          *struct {
			Email    string  `json:"email"`
			AuthMode string  `json:"auth_mode"`
			Tier     string  `json:"subscription_tier"`
			Team     *string `json:"team_name"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Authenticated == nil {
		return harness.AccountSnapshot{}, false
	}
	a := harness.AccountSnapshot{Observation: observed, LoggedIn: r.Authenticated}
	if !*r.Authenticated || r.Meta == nil {
		return a, true
	}
	a.Email, a.AuthMethod, a.Plan = r.Meta.Email, r.Meta.AuthMode, r.Meta.Tier
	if r.Meta.Team != nil {
		a.Organization = *r.Meta.Team
	}
	return a, true
}
