package native

import (
	"os"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

// AntigravityOptions are Antigravity's own execution options.
type AntigravityOptions struct {
	// PermissionMode is passed as --mode: "accept-edits", "auto" or "plan".
	// Empty passes no --mode, so agy's own default applies; agy 1.4.2 names
	// that default "request-review" but does not accept it as a --mode value.
	PermissionMode string
	// Agent selects the agent profile (--agent).
	Agent string
}

func (o AntigravityOptions) set() bool { return o != AntigravityOptions{} }

func (o AntigravityOptions) validMode() bool {
	switch o.PermissionMode {
	case "", "accept-edits", "auto", "plan":
		return true
	}
	return false
}

// withAntigravityEnvironment strips inherited parent Antigravity language-server
// attachment variables so the child starts its own run, and applies non-interactive
// defaults. Nil means the inherited environment when no stripping or override is needed.
func withAntigravityEnvironment(env []string) []string {
	source := env
	if source == nil {
		source = os.Environ()
	}
	out := make([]string, 0, len(source)+len(nativecli.AntigravityReducedTelemetry))
	for _, entry := range source {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ANTIGRAVITY_") || strings.HasPrefix(upper, "JETSKI_") {
			continue
		}
		out = append(out, entry)
	}
	return nativecli.Override(out, nativecli.AntigravityReducedTelemetry...)
}

// antigravityArgs binds values with --flag=value so a prompt or value beginning
// with "-" is never misread as another flag.
func antigravityArgs(c Config, r Request) []string {
	args := []string{"-p=" + r.Prompt, "--output-format=stream-json"}
	option := func(name, value string) {
		if value != "" {
			args = append(args, "--"+name+"="+value)
		}
	}
	option("conversation", r.ResumeSession)
	if r.Schema != "" {
		option("json-schema", jsonCompact(r.Schema))
	}
	option("model", c.Model)
	option("effort", c.Effort)
	option("mode", c.Antigravity.PermissionMode)
	option("agent", c.Antigravity.Agent)
	return append(args, c.Args...)
}
