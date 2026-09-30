package native

import (
	"encoding/json"
	"math"
	"slices"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
)

// CodexOptions are Codex's own execution options.
type CodexOptions struct {
	// Sandbox is passed as --sandbox, or as a sandbox_mode override on resume.
	Sandbox string
}

// ClaudeOptions are Claude's own execution options.
type ClaudeOptions struct {
	PermissionMode string
	// AllowedTools are permission allow rules. Empty adds none; it does not
	// remove tools.
	AllowedTools []string
	// MaxBudgetUSD caps the API-rate valuation of one invocation; zero is no cap.
	MaxBudgetUSD float64
}

// GrokOptions are Grok's own execution options.
type GrokOptions struct {
	Sandbox        string
	PermissionMode string
	// Tools are the built-in tools to allow. Nil keeps Grok's default set. An
	// empty non-nil list is refused: Grok has no verified way to say "none",
	// and omitting the flag would allow every tool.
	Tools     []string
	Telemetry GrokTelemetryPolicy
}

func (o CodexOptions) set() bool { return o != CodexOptions{} }
func (o ClaudeOptions) set() bool {
	return o.PermissionMode != "" || o.AllowedTools != nil || o.MaxBudgetUSD != 0
}
func (o GrokOptions) set() bool {
	return o.Sandbox != "" || o.PermissionMode != "" || o.Tools != nil || o.Telemetry != GrokTelemetryDefault
}

// validate refuses, before launch, everything the selected engine would
// otherwise ignore or misread.
func validate(c Config, r Request) *RunError {
	engine := c.Provider.Engine
	if !harness.Support(engine, harness.Run, harness.Available).Usable() {
		return capabilityError(engine, CodeUnsupportedEngine)
	}
	if code := c.Provider.Problem(); code != "" {
		return capabilityError(engine, code)
	}
	if code := optionProblem(c); code != "" {
		return capabilityError(engine, code)
	}
	if code := requestProblem(engine, c, r); code != "" {
		return capabilityError(engine, code)
	}
	if managedFlagIn(engine, c.Args) || (engine == harness.Codex && c.Browser && codexBrowserOverride(c.Args)) {
		return capabilityError(engine, CodeManagedFlagInArgs)
	}
	if _, _, err := planSkills(c); err != nil {
		return err
	}
	return nil
}

func optionProblem(c Config) string {
	engine := c.Provider.Engine
	if (engine != harness.Codex && c.Codex.set()) || (engine != harness.Claude && c.Claude.set()) || (engine != harness.Grok && c.Grok.set()) {
		return CodeOptionForOtherEngine
	}
	switch engine {
	case harness.Claude:
		if b := c.Claude.MaxBudgetUSD; b < 0 || math.IsNaN(b) || math.IsInf(b, 0) {
			return CodeUnsupportedOption
		}
	case harness.Grok:
		if !c.Grok.Telemetry.valid() {
			return CodeUnsupportedOption
		}
		if c.Grok.Tools != nil && len(c.Grok.Tools) == 0 {
			return CodeUnsupportedOption
		}
		for _, tool := range c.Grok.Tools {
			if tool == "" || strings.Contains(tool, ",") {
				return CodeUnsupportedOption
			}
		}
	}
	return ""
}

// requestProblem asks the support table rather than comparing engine names.
func requestProblem(engine harness.Engine, c Config, r Request) string {
	wanted := []struct {
		on      bool
		feature harness.Feature
	}{
		{r.Schema != "", harness.StructuredOutput},
		{r.ResumeSession != "", harness.Resume},
		{c.Effort != "", harness.Effort},
		{c.Browser, harness.Browser},
		{c.Background, harness.Background},
	}
	for _, w := range wanted {
		if w.on && !harness.Support(engine, harness.Run, w.feature).Usable() {
			return CodeUnsupportedOption
		}
	}
	if r.Schema != "" && !json.Valid([]byte(r.Schema)) {
		return CodeInvalidSchema
	}
	return ""
}

// managedFlags are the flags each engine's argv builder owns, as spelled by
// codex-cli exec, Claude Code and grok --help. Short options match with a
// separate, attached or =-joined value.
var managedFlags = map[harness.Engine]struct {
	long  []string
	short string
}{
	harness.Codex: {
		long:  []string{"output-schema", "output-last-message", "json", "sandbox", "cd", "model"},
		short: "osCm",
	},
	harness.Claude: {
		long:  []string{"json-schema", "output-format", "permission-mode", "allowedTools", "allowed-tools", "max-budget-usd", "resume", "model", "effort", "append-system-prompt", "chrome", "no-chrome"},
		short: "r",
	},
	harness.Grok: {
		long:  []string{"single", "output-format", "json-schema", "tools", "sandbox", "permission-mode", "resume", "cwd", "model", "reasoning-effort", "effort", "rules"},
		short: "prm",
	},
}

// codexManagedOverrides are the config keys Codex's argv builder sets with -c.
var codexManagedOverrides = []string{"sandbox_mode", "developer_instructions", "model_reasoning_effort", "model"}

// managedFlagIn reports whether Args repeats a flag the library sets, or a
// "--" that would turn the library's own arguments into positionals.
func managedFlagIn(engine harness.Engine, args []string) bool {
	flags := managedFlags[engine]
	for i, arg := range args {
		if arg == "--" {
			return true
		}
		if name, ok := strings.CutPrefix(arg, "--"); ok {
			name, _, _ = strings.Cut(name, "=")
			if slices.Contains(flags.long, name) {
				return true
			}
			if engine == harness.Codex && name == "config" && codexManagedOverride(arg, "--config", args, i) {
				return true
			}
			continue
		}
		if len(arg) < 2 || arg[0] != '-' {
			continue
		}
		if strings.IndexByte(flags.short, arg[1]) >= 0 {
			return true
		}
		if engine == harness.Codex && arg[1] == 'c' && codexManagedOverride(arg, "-c", args, i) {
			return true
		}
	}
	return false
}

// codexManagedOverride reads the key of a -c/--config override, whose value is
// the next argument, or attached directly or after "=".
func codexManagedOverride(arg, flag string, args []string, i int) bool {
	return slices.Contains(codexManagedOverrides, codexOverrideKey(arg, flag, args, i))
}

func codexOverrideKey(arg, flag string, args []string, i int) string {
	value := strings.TrimPrefix(strings.TrimPrefix(arg, flag), "=")
	if arg == flag {
		if i+1 >= len(args) {
			return ""
		}
		value = args[i+1]
	}
	key, _, _ := strings.Cut(value, "=")
	parts := strings.Split(key, ".")
	for i := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"'`)
	}
	return strings.Join(parts, ".")
}

// Browser owns only these feature switches, and only while opted in. Keep
// ordinary callers' existing feature overrides available.
func codexBrowserOverride(args []string) bool {
	for i, arg := range args {
		flag := ""
		switch {
		case strings.HasPrefix(arg, "--config=") || arg == "--config":
			flag = "--config"
		case strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--"):
			flag = "-c"
		}
		if flag != "" {
			switch codexOverrideKey(arg, flag, args, i) {
			case "features", "features.browser_use", "features.browser_use_external":
				return true
			}
		}
		for _, featureFlag := range []string{"--enable", "--disable"} {
			value, joined := strings.CutPrefix(arg, featureFlag+"=")
			if arg == featureFlag && i+1 < len(args) {
				value, joined = args[i+1], true
			}
			if joined {
				for _, feature := range strings.Split(value, ",") {
					if feature == "browser_use" || feature == "browser_use_external" {
						return true
					}
				}
			}
		}
	}
	return false
}
