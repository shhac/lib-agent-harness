package nativecli

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
