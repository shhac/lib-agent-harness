package native

import (
	"encoding/json"
	"math"
	"slices"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

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
	if engine == harness.Antigravity {
		if code := nativecli.AntigravityHomeProblem(c.Provider.CLI.Home); code != "" {
			return capabilityError(engine, code)
		}
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
	for other, entry := range nativeEngines {
		if other != engine && entry.optionsSet(c) {
			return CodeOptionForOtherEngine
		}
	}
	switch engine {
	case harness.Claude:
		if b := c.Claude.MaxBudgetUSD; b < 0 || math.IsNaN(b) || math.IsInf(b, 0) {
			return CodeUnsupportedOption
		}
	case harness.Antigravity:
		if !c.Antigravity.validMode() {
			return CodeUnsupportedOption
		}
	case harness.Fx:
		switch c.Fx.PermissionMode {
		case "", "ask", "auto", "full-access":
		default:
			return CodeUnsupportedOption
		}
		// fx reads its profile from the user's home directory and has no
		// variable that moves it, so a home that names another directory
		// cannot be honoured and is refused rather than ignored.
		if c.Provider.CLI.Home != "" {
			if code := nativecli.FxHomeProblem(c.Provider.CLI.Home); code != "" {
				return CodeHomeUnsupported
			}
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
	if engine == harness.Antigravity && r.AppendInstructions != "" {
		return CodeUnsupportedOption
	}
	if r.Schema != "" && !json.Valid([]byte(r.Schema)) {
		return CodeInvalidSchema
	}
	return ""
}

// managedFlags are the flags each engine's argv builder owns, as spelled by
// codex-cli exec, Claude Code, grok --help, and agy --help. Short options match
// with a separate, attached or =-joined value.
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
	harness.Antigravity: {
		long:  []string{"print", "output-format", "input-format", "json-schema", "model", "effort", "mode", "dangerously-skip-permissions", "sandbox", "agent", "conversation"},
		short: "pmc",
	},
	harness.Fx: {
		long:  []string{"json", "model", "effort", "resume", "resume-id", "auto", "full-access", "yolo", "system", "quiet", "no-save", "prompt-permissions", "continue-recovery"},
		short: "",
	},
}

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
