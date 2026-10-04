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
	if overrides := engines[o.Provider.Engine].overrides; overrides != nil {
		env = overrides(o, env)
	}
	return append(env, o.Env...)
}

// claudeOverrides turns off auto-memory in a sandboxed session: it would read
// and write the operator's own memory folders.
func claudeOverrides(o Options, env []string) []string {
	if o.Sandbox == nil {
		return env
	}
	return append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
}

func grokOverrides(o Options, env []string) []string {
	if o.Policy.GrokTelemetry != GrokTelemetryReduced {
		return env
	}
	return nativecli.Override(env, nativecli.GrokReducedTelemetry...)
}

// homeVariable names the environment variable that selects an engine's home.
func homeVariable(e harness.Engine) string { return engines[e].homeVariable }

// codexHomeValue gives a restricted or sandboxed session its own runtime
// home: the library wrote that home's configuration and shared the login into
// it, so nothing the operator keeps beside their credential comes along.
func codexHomeValue(o Options) (string, bool) {
	if (o.Restriction != nil || o.Sandbox != nil) && o.RuntimeHome != "" {
		return o.RuntimeHome, true
	}
	return o.Provider.CLI.Home, true
}

// claudeHomeValue leaves CLAUDE_CONFIG_DIR unset for the CLI's own default
// home, where setting it would move Claude's project and credential lookup.
func claudeHomeValue(o Options) (string, bool) {
	home, err := os.UserHomeDir()
	if err == nil && filepath.Clean(o.Provider.CLI.Home) == filepath.Join(home, ".claude") {
		return "", false
	}
	return o.Provider.CLI.Home, true
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
		if withheld := engines[o.Provider.Engine].withheld; withheld != nil && withheld(key) {
			continue
		}
		if providerManaged(key) {
			continue
		}
		env = append(env, entry)
	}
	entry := engines[o.Provider.Engine]
	if entry.homeVariable == "" {
		return env
	}
	selected, set := o.Provider.CLI.Home, true
	if entry.homeValue != nil {
		selected, set = entry.homeValue(o)
	}
	if !set {
		return env
	}
	return append(env, entry.homeVariable+"="+selected)
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
		}
		if refuseAddition := engines[o.Provider.Engine].refuseAddition; refuseAddition != nil {
			if err := refuseAddition(o, key, upper); err != nil {
				return err
			}
		}
	}
	return nil
}

func refuseCodexAddition(o Options, key, upper string) error {
	if o.Sandbox != nil && o.Browser && (strings.HasPrefix(upper, "NODE_REPL_") || strings.HasPrefix(upper, "BROWSER_USE_") || strings.HasPrefix(upper, "SKY_")) {
		return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change the sandboxed browser outside its proven configuration")
	}
	return nil
}

func refuseCommandCodeAddition(o Options, key, _ string) error {
	if commandCodeManaged(key) {
		return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change how Command Code runs outside the session's policy")
	}
	return nil
}

// refuseGrokAddition refuses every GROK_ and XAI_ addition, more than a Grok
// session withholds on inheritance: the operator's own Grok settings carry
// over, but a caller sets Grok's controls only through Policy.
func refuseGrokAddition(o Options, key, upper string) error {
	if strings.HasPrefix(upper, "GROK_") || strings.HasPrefix(upper, "XAI_") {
		return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" is managed by the harness; set Policy.GrokTelemetry for Grok's telemetry controls")
	}
	return nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
