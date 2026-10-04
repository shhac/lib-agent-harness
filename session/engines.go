package session

import (
	"context"
	"encoding/json"

	harness "github.com/shhac/lib-agent-harness"
)

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
	// initialize starts or resumes the engine's conversation on a live wire,
	// checking what the engine reports before any turn.
	initialize func(s *Session, ctx context.Context, resume bool) error
	// sessionNotice handles a notification about the session itself before
	// any turn sees it, such as a start-up surface or a mode change, and
	// reports whether it consumed it.
	sessionNotice func(s *Session, m map[string]json.RawMessage) bool
	// event handles a notification for the active turn.
	event func(s *Session, t *Turn, ref Ref, m map[string]json.RawMessage)
}

var engines = map[harness.Engine]engineEntry{
	harness.Codex: {
		dialect: codexDialect, binary: "codex",
		homeVariable: "CODEX_HOME", homeDir: ".codex", homeValue: codexHomeValue,
		normalizePolicy: normalizeCodexPolicy, refuseAddition: refuseCodexAddition,
		initialize: (*Session).initializeCodex, event: (*Session).codexEvent,
	},
	harness.Claude: {
		dialect: claudeDialect, binary: "claude",
		homeVariable: "CLAUDE_CONFIG_DIR", homeDir: ".claude", homeValue: claudeHomeValue,
		normalizePolicy: normalizeClaudePolicy, overrides: claudeOverrides,
		initialize: (*Session).initializeClaude, sessionNotice: (*Session).observeClaudeInit, event: (*Session).claudeEvent,
	},
	harness.Grok: {
		dialect: grokDialect, binary: "grok",
		homeVariable: "GROK_HOME", homeDir: ".grok",
		normalizePolicy: normalizeGrokPolicy, withheld: grokManaged,
		refuseAddition: refuseGrokAddition, overrides: grokOverrides,
		initialize: (*Session).initializeGrok, event: (*Session).grokEvent,
	},
	harness.CommandCode: {
		dialect: commandCodeDialect, binary: "cmd", resolveHome: commandCodeHome,
		normalizePolicy: normalizeCommandCodePolicy, withheld: commandCodeManaged,
		refuseAddition: refuseCommandCodeAddition,
		initialize:     (*Session).initializeCommandCode, sessionNotice: (*Session).commandCodeModeUpdate, event: (*Session).commandCodeEvent,
	},
}
