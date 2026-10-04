package session

import harness "github.com/shhac/lib-agent-harness"

// engineEntry is what the session package knows about one CLI engine. Every
// engine-specific decision moves here as the design in
// design-docs/2026-10-04-engine-adapters.md lands, so that adding an engine
// is adding its entry rather than editing every place that switches on it.
// An entry holds no session state.
type engineEntry struct {
	// dialect frames the engine's wire under the session's policy.
	dialect func(Policy) dialect
}

var engines = map[harness.Engine]engineEntry{
	harness.Codex:       {dialect: codexDialect},
	harness.Claude:      {dialect: claudeDialect},
	harness.Grok:        {dialect: grokDialect},
	harness.CommandCode: {dialect: commandCodeDialect},
}
