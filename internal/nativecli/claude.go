package nativecli

// ClaudeRestrictedArgs removes Claude's native tools, instructions, hooks, MCP
// servers, slash commands and session persistence. Callers append their own
// mode flags.
func ClaudeRestrictedArgs() []string {
	return []string{"--safe-mode", "--setting-sources=", "--settings={\"disableAllHooks\":true}", "--strict-mcp-config", "--mcp-config={\"mcpServers\":{}}", "--tools=", "--disable-slash-commands", "--no-chrome", "--no-session-persistence", "--permission-mode", "dontAsk"}
}
