package native

import (
	"os"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

// GrokTelemetryPolicy selects Grok's client-side telemetry policy, as
// GrokOptions.Telemetry. Default leaves Grok's environment and
// behaviour unchanged. Reduced opts out of Grok product telemetry and related
// background discovery that can read local provider configuration; it does not
// prevent the model request, configured tools, or other required provider
// traffic from leaving the machine, and it cannot change account-level
// data-sharing or retention settings, which live with the xAI account.
type GrokTelemetryPolicy uint8

const (
	GrokTelemetryDefault GrokTelemetryPolicy = iota
	GrokTelemetryReduced
)

func (p GrokTelemetryPolicy) valid() bool {
	return p == GrokTelemetryDefault || p == GrokTelemetryReduced
}

// grokReducedTelemetryEnvironment is shared with every other package that
// launches Grok, so no launch can fall behind a new import switch.
var grokReducedTelemetryEnvironment = nativecli.GrokReducedTelemetry

// withGrokReducedTelemetry overlays the reduced policy onto env; nil means
// the inherited environment.
func withGrokReducedTelemetry(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	for _, entry := range grokReducedTelemetryEnvironment {
		key, value, _ := strings.Cut(entry, "=")
		env = overrideEnv(env, key, value)
	}
	return env
}

// grokArgs binds every value with --flag=value: Grok's parser reads a separate
// value beginning with "-" as a missing value, so `-p -x` fails to start.
func grokArgs(c Config, r Request) []string {
	args := []string{"--single=" + r.Prompt, "--output-format=streaming-json"}
	option := func(name, value string) {
		if value != "" {
			args = append(args, "--"+name+"="+value)
		}
	}
	option("resume", r.ResumeSession)
	option("cwd", r.WorkDir)
	option("rules", r.AppendInstructions)
	if r.Schema != "" {
		option("json-schema", jsonCompact(r.Schema))
	}
	option("model", c.Model)
	option("reasoning-effort", c.Effort)
	option("sandbox", c.Grok.Sandbox)
	option("permission-mode", c.Grok.PermissionMode)
	option("tools", strings.Join(c.Grok.Tools, ","))
	return append(args, c.Args...)
}
