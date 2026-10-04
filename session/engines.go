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
	// The default binary, home variable and home folder are the engine's
	// harness.Descriptor. resolveHome, when set, replaces resolving the home
	// from them.
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
	// bufferStart holds a turn's notifications until the engine answers the
	// request that started it, because they name a turn not yet known.
	bufferStart bool
	// startTurn sends the turn's input.
	startTurn func(s *Session, request, lifetime context.Context, t *Turn, ref Ref, in Input) error
	// interrupt asks the engine to cancel the expected turn.
	interrupt func(s *Session, ctx context.Context, expected string) error
	// steer is the engine's native steering; nil steers by interrupting and
	// starting another turn, for the reason composedSteer gives.
	steer         func(s *Session, ctx context.Context, t *Turn, expected string, in Input) (SteerResult, error)
	composedSteer string
	// account, quota and context are the engine's native telemetry reads;
	// accountFromStart serves the account its start-up handshake reported.
	// args is the CLI's command line for a session, and restrictedArgs what
	// a restricted session adds once its tool host is open; nil refuses
	// restriction.
	args           func(o Options, nativeID string, resuming bool, l *launch) []string
	restrictedArgs func(ctx context.Context, o Options, host *toolHost) (Options, []string, error)
	// sandbox is how the engine runs sandboxed; nil refuses a sandbox.
	sandbox *sandboxSupport
	// foreignPolicy refuses Policy fields belonging to other engines.
	foreignPolicy func(Options) error
	// inspect reads a login without a conversation; nil refuses Inspect,
	// pointing elsewhere when inspectElsewhere says where.
	inspect          *inspectLaunch
	inspectElsewhere string
	account          *accountRead
	accountFromStart bool
	quota            *quotaRead
	context          *contextRead
}

// engines is filled in init: its entries refer to functions that themselves
// look an engine up, which Go does not allow in a variable's initializer.
var engines map[harness.Engine]engineEntry

func init() {
	engines = map[harness.Engine]engineEntry{
		harness.Codex: {
			dialect: codexDialect, homeValue: codexHomeValue,
			args: codexArgs, restrictedArgs: codexRestrictedLaunch, sandbox: codexSandbox,
			foreignPolicy: codexForeignPolicy, inspect: codexInspect,
			normalizePolicy: normalizeCodexPolicy, refuseAddition: refuseCodexAddition,
			initialize: (*Session).initializeCodex, event: (*Session).codexEvent,
			bufferStart: true, startTurn: (*Session).startCodexTurnSynced,
			interrupt: (*Session).interruptCodex, steer: (*Session).steerCodex,
			account: codexAccountRead, quota: codexQuotaRead,
		},
		harness.Claude: {
			dialect: claudeDialect, homeValue: claudeHomeValue,
			args: claudeArgs, restrictedArgs: claudeRestrictedLaunch, sandbox: claudeSandbox,
			foreignPolicy: claudeForeignPolicy, inspect: claudeInspect,
			normalizePolicy: normalizeClaudePolicy, overrides: claudeOverrides,
			initialize: (*Session).initializeClaude, sessionNotice: (*Session).observeClaudeInit, event: (*Session).claudeEvent,
			startTurn: (*Session).startClaudeTurn, interrupt: (*Session).interruptClaude,
			composedSteer:    "Claude steering interrupts and starts another turn",
			accountFromStart: true, quota: claudeQuotaRead, context: claudeContextRead,
		},
		harness.Grok: {
			dialect:       grokDialect,
			foreignPolicy: grokForeignPolicy, inspectElsewhere: "inspect a Grok login with account.Inspect",
			args:            func(o Options, _ string, _ bool, _ *launch) []string { return grokArgs(o) },
			normalizePolicy: normalizeGrokPolicy, withheld: grokManaged,
			refuseAddition: refuseGrokAddition, overrides: grokOverrides,
			initialize: (*Session).initializeGrok, event: (*Session).grokEvent,
			startTurn: (*Session).startGrokTurn, interrupt: (*Session).interruptACP,
			composedSteer: "Grok steering cancels the running prompt and sends another",
			account:       grokAccountRead,
		},
		harness.CommandCode: {
			dialect: commandCodeDialect, resolveHome: commandCodeHome,
			foreignPolicy:   commandCodeForeignPolicy,
			args:            func(Options, string, bool, *launch) []string { return commandCodeArgs() },
			normalizePolicy: normalizeCommandCodePolicy, withheld: commandCodeManaged,
			refuseAddition: refuseCommandCodeAddition,
			initialize:     (*Session).initializeCommandCode, sessionNotice: (*Session).commandCodeModeUpdate, event: (*Session).commandCodeEvent,
			startTurn: (*Session).startCommandCodeTurn, interrupt: (*Session).interruptACP,
			composedSteer: "Command Code steering cancels the running prompt and sends another",
		},
		// The OpenAI-compatible engine runs the library's own loop, with no
		// process wire; it shares only the turn lifecycle.
		harness.OpenAICompatible: {
			startTurn: (*Session).startAPITurnEntry, interrupt: (*Session).interruptAPI,
			composedSteer: "the library interrupts the turn and starts another",
		},
	}
}
