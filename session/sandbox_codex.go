package session

// codexSandboxArgs are the overrides a sandboxed Codex harness and its canary
// both run with. They are the only place the profile is described.
func codexSandboxArgs(s Sandbox) []string {
	workspace := "read"
	if s.Write {
		workspace = "write"
	}
	// Codex searches through its provider, not from the shell, so a live
	// search leaves the profile's network closed.
	webSearch := `web_search="disabled"`
	if s.Web {
		webSearch = `web_search="live"`
	}
	agents := `agents.enabled=false`
	if s.Subagents {
		agents = `agents.enabled=true`
	}
	settings := []string{
		`default_permissions="` + sandboxProfile + `"`,
		// Configuration a later launch would read is never writable: .git for
		// hooks, .codex and .agents for servers and skills that run outside it.
		`permissions.` + sandboxProfile + `.filesystem={":root"="read",":workspace_roots"={"."="` + workspace + `",".git"="read",".codex"="read",".agents"="read"}}`,
		`permissions.` + sandboxProfile + `.network.enabled=false`,
		`approval_policy="never"`,
		webSearch,
		agents,
		`check_for_update_on_startup=false`,
		`analytics.enabled=false`,
	}
	// Surfaces that reach past the sandbox — connectors, plugins, hooks, a
	// browser or the desktop — are switched off. Browser opts into exactly
	// two browser features after its private bridge is assembled. The shell
	// and file tools stay,
	// and so does the code-mode host: on codex 0.154.0 every native tool call
	// runs through it, and disabling it leaves a session unable to do anything.
	for _, feature := range sandboxDisabledFeatures {
		if s.Subagents && feature == codexSubagentFeature {
			continue
		}
		settings = append(settings, "features."+feature+"=false")
	}
	if s.Subagents {
		settings = append(settings, "features."+codexSubagentFeature+"=true")
	}
	return codexOverrides(settings)
}

// codexSubagentFeature is codex-cli 0.166.0's stable multi-agent feature.
// Codex documents its agents as inheriting the session's sandbox, approval
// policy and permissions; the library does not prove it. multi_agent_v2
// stays off.
const codexSubagentFeature = "multi_agent"
