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
	// binary is the CLI a session runs when the caller names none.
	binary string
	// homeVariable selects the engine's home in its environment; empty when
	// the engine has none and finds its home itself.
	homeVariable string
	// homeDir is the default home, under the user's home directory, when
	// homeVariable is unset in this process.
	homeDir string
	// resolveHome, when set, replaces that resolution entirely.
	resolveHome func(Options) (Options, error)
	// homeValue is the value given homeVariable, and false to leave it
	// unset; nil gives the caller's home.
	homeValue func(Options) (string, bool)
	// normalizePolicy applies the engine's policy defaults and refuses a value
	// it does not recognise.
	normalizePolicy func(Options) (Options, error)
	// withheld reports a variable this engine's sessions do not inherit,
	// beyond those no session inherits.
	withheld func(key string) bool
	// refuseAddition refuses a caller's environment addition the engine
	// manages itself, or returns nil. It is a separate rule from withheld:
	// an engine may inherit what a caller may not add.
	refuseAddition func(o Options, key, upper string) error
	// overrides adjusts the inherited environment before the caller's
	// additions are appended.
	overrides func(o Options, env []string) []string
}

var engines = map[harness.Engine]engineEntry{
	harness.Codex: {
		dialect: codexDialect, binary: "codex",
		homeVariable: "CODEX_HOME", homeDir: ".codex", homeValue: codexHomeValue,
		normalizePolicy: normalizeCodexPolicy, refuseAddition: refuseCodexAddition,
	},
	harness.Claude: {
		dialect: claudeDialect, binary: "claude",
		homeVariable: "CLAUDE_CONFIG_DIR", homeDir: ".claude", homeValue: claudeHomeValue,
		normalizePolicy: normalizeClaudePolicy, overrides: claudeOverrides,
	},
	harness.Grok: {
		dialect: grokDialect, binary: "grok",
		homeVariable: "GROK_HOME", homeDir: ".grok",
		normalizePolicy: normalizeGrokPolicy, withheld: grokManaged,
		refuseAddition: refuseGrokAddition, overrides: grokOverrides,
	},
	harness.CommandCode: {
		dialect: commandCodeDialect, binary: "cmd", resolveHome: commandCodeHome,
		normalizePolicy: normalizeCommandCodePolicy, withheld: commandCodeManaged,
		refuseAddition: refuseCommandCodeAddition,
	},
}
