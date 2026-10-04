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
	settings := []string{
		`default_permissions="` + sandboxProfile + `"`,
		// Configuration a later launch would read is never writable: .git for
		// hooks, .codex and .agents for servers and skills that run outside it.
		`permissions.` + sandboxProfile + `.filesystem={":root"="read",":workspace_roots"={"."="` + workspace + `",".git"="read",".codex"="read",".agents"="read"}}`,
		`permissions.` + sandboxProfile + `.network.enabled=false`,
		`approval_policy="never"`,
		webSearch,
		`agents.enabled=false`,
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
		settings = append(settings, "features."+feature+"=false")
	}
	return codexOverrides(settings)
}
