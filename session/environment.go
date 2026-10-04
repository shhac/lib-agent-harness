package session

// A session's child environment: what it inherits, what it is given, and
// what a caller may add.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

// environment is the session's own environment plus the caller's additions,
// which come last so they take effect.
func environment(o Options) []string {
	env := baseEnvironment(o)
	if o.Sandbox != nil && o.Provider.Engine == harness.Claude {
		// Auto-memory would read and write the operator's own memory folders.
		env = append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
	}
	if o.Provider.Engine == harness.Grok && o.Policy.GrokTelemetry == GrokTelemetryReduced {
		env = nativecli.Override(env, nativecli.GrokReducedTelemetry...)
	}
	return append(env, o.Env...)
}

// homeVariable names the environment variable that selects an engine's home.
func homeVariable(e harness.Engine) string {
	switch e {
	case harness.Claude:
		return "CLAUDE_CONFIG_DIR"
	case harness.Grok:
		return "GROK_HOME"
	case harness.CommandCode:
		// Command Code derives its home from the user's home directory.
		return ""
	}
	return "CODEX_HOME"
}

// grokManaged is what a Grok session never inherits: its home, which the
// session selects, and the credentials and endpoint overrides that would move
// it off the selected login.
func grokManaged(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "XAI_") || strings.HasPrefix(upper, "GROK_AUTH") {
		return true
	}
	if !strings.HasPrefix(upper, "GROK_") {
		return false
	}
	return upper == "GROK_HOME" || upper == "GROK_DEPLOYMENT_KEY" || upper == "GROK_AGENT_SECRET" || strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_URL")
}

// commandCodeManaged is what a Command Code session neither inherits nor
// accepts as an addition: Command Code's own variables, which change where it
// connects, which providers it uses and how it runs, except its protective
// switches.
func commandCodeManaged(key string) bool {
	upper := strings.ToUpper(key)
	if upper == "CMD_ZDR" || upper == "CMD_LOCAL_ONLY" {
		return false
	}
	return strings.HasPrefix(upper, "CMD_") || strings.HasPrefix(upper, "COMMANDCODE_") || strings.HasPrefix(upper, "COMMAND_CODE_")
}

// sandboxInherited is all a sandboxed session inherits from this process:
// enough to find the CLI, its login and a shell, and nothing the process
// happened to have exported.
var sandboxInherited = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true, "LANG": true, "TZ": true, "TMPDIR": true, "__CF_USER_TEXT_ENCODING": true}

func sandboxInherits(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	return sandboxInherited[key] || strings.HasPrefix(key, "LC_")
}

func baseEnvironment(o Options) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if o.Sandbox != nil && !sandboxInherits(entry) {
			continue
		}
		key, _, _ := strings.Cut(entry, "=")
		// Retain USER and other OS identity variables: native keychain lookup uses
		// them. Strip provider credentials/overrides to preserve subscription login.
		if o.Provider.Engine == harness.Grok && grokManaged(key) {
			continue
		}
		if o.Provider.Engine == harness.CommandCode && commandCodeManaged(key) {
			continue
		}
		if providerManaged(key) {
			continue
		}
		env = append(env, entry)
	}
	// A restricted session runs in its own home: the library wrote that home's
	// configuration and shared the login into it, so nothing the operator keeps
	// beside their credential comes along.
	selected := o.Provider.CLI.Home
	if (o.Restriction != nil || o.Sandbox != nil) && o.RuntimeHome != "" && o.Provider.Engine == harness.Codex {
		selected = o.RuntimeHome
	}
	key := homeVariable(o.Provider.Engine)
	if key == "" {
		return env
	}
	if o.Provider.Engine == harness.Claude {
		home, err := os.UserHomeDir()
		if err == nil && filepath.Clean(selected) == filepath.Join(home, ".claude") {
			return env
		}
	}
	return append(env, key+"="+selected)
}

// providerManaged is what no session inherits: the homes the harness selects
// and the credentials and provider overrides that would bypass the shared
// subscription login.
func providerManaged(key string) bool {
	switch {
	case key == "CODEX_HOME", key == "CLAUDE_CONFIG_DIR", key == "CLAUDECODE",
		key == "OPENAI_API_KEY", key == "OPENAI_BASE_URL",
		key == "ANTHROPIC_API_KEY", key == "ANTHROPIC_AUTH_TOKEN", key == "ANTHROPIC_BASE_URL", key == "ANTHROPIC_MODEL",
		key == "CLAUDE_CODE_OAUTH_TOKEN",
		strings.HasPrefix(key, "CLAUDE_CODE_USE_"), strings.HasPrefix(key, "ANTHROPIC_DEFAULT_"):
		return true
	}
	return false
}

// validateEnv refuses additions the harness manages itself: credentials and
// provider overrides it strips, the homes it selects, and the process basics
// an addition could use to change which binaries or login are used.
func validateEnv(o Options) error {
	for _, entry := range o.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !envKey.MatchString(key) {
			return refuse(o, "env", RefusedEnvMalformed, "environment additions must be KEY=VALUE")
		}
		upper := strings.ToUpper(key)
		switch {
		case key == "HOME", key == "PATH", key == "USER", key == "LOGNAME", key == "SHELL", key == "CLAUDECODE",
			strings.HasPrefix(upper, "CODEX_"), strings.HasPrefix(upper, "CLAUDE_"), strings.HasPrefix(upper, "ANTHROPIC_"), strings.HasPrefix(upper, "OPENAI_"),
			// These change what the CLI itself loads or where it connects, and
			// it runs outside the sandbox.
			strings.HasPrefix(upper, "DYLD_"), strings.HasPrefix(upper, "LD_"), upper == "NODE_OPTIONS", upper == "NODE_PATH", strings.HasPrefix(upper, "BUN_"),
			strings.HasSuffix(upper, "_PROXY"), strings.HasPrefix(upper, "SSL_CERT_"), upper == "NODE_EXTRA_CA_CERTS", strings.HasPrefix(upper, "GIT_"):
			return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" is managed by the harness or would change the CLI outside its sandbox")
		case o.Provider.Engine == harness.Codex && o.Sandbox != nil && o.Browser &&
			(strings.HasPrefix(upper, "NODE_REPL_") || strings.HasPrefix(upper, "BROWSER_USE_") || strings.HasPrefix(upper, "SKY_")):
			return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change the sandboxed browser outside its proven configuration")
		}
		if o.Provider.Engine == harness.CommandCode && commandCodeManaged(key) {
			return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change how Command Code runs outside the session's policy")
		}
		if o.Provider.Engine == harness.Grok && (strings.HasPrefix(upper, "GROK_") || strings.HasPrefix(upper, "XAI_")) {
			return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" is managed by the harness; set Policy.GrokTelemetry for Grok's telemetry controls")
		}
	}
	return nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
