package session

// A session's child environment: what it inherits, what it is given, and
// what a caller may add.

import (
	"os"
	"regexp"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
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

// homeVariable names the environment variable that selects an engine's home.
func homeVariable(e harness.Engine) string {
	described, _ := harness.Describe(e)
	return described.HomeVariable
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
	key := homeVariable(o.Provider.Engine)
	if key == "" {
		return env
	}
	selected, set := o.Provider.CLI.Home, true
	if entry.homeValue != nil {
		selected, set = entry.homeValue(o)
	}
	if !set {
		return env
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
		}
		if refuseAddition := engines[o.Provider.Engine].refuseAddition; refuseAddition != nil {
			if err := refuseAddition(o, key, upper); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuseManagedAddition refuses a caller's addition of a variable managed
// reports, for an engine whose inheritance and addition rules are the same
// set. Each engine wires it explicitly, so changing what an engine inherits
// is never also a silent change to what a caller may add.
func refuseManagedAddition(label string, managed func(key string) bool) func(o Options, key, upper string) error {
	return func(o Options, key, _ string) error {
		if managed(key) {
			return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change how "+label+" runs outside the session's policy")
		}
		return nil
	}
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
