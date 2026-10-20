package native

import (
	"slices"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/restrict"
)

// CodexOptions are Codex's own execution options.
type CodexOptions struct {
	// Sandbox is passed as --sandbox, or as a sandbox_mode override on resume.
	Sandbox string
}

func (o CodexOptions) set() bool { return o != CodexOptions{} }

func codexArgs(c Config, r Request, report *codexReport) []string {
	args := []string{"exec"}
	if r.ResumeSession != "" {
		args = append(args, "resume")
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	args = append(args, "--json")
	if r.ResumeSession == "" {
		if c.Codex.Sandbox != "" {
			args = append(args, "--sandbox", c.Codex.Sandbox)
		}
		if r.WorkDir != "" {
			args = append(args, "--cd", r.WorkDir)
		}
		args = append(args, "--skip-git-repo-check")
	} else {
		args = append(args, "--skip-git-repo-check")
		if c.Codex.Sandbox != "" {
			args = append(args, "-c", codexOverride("sandbox_mode", c.Codex.Sandbox))
		}
	}
	if r.AppendInstructions != "" {
		args = append(args, "-c", codexOverride("developer_instructions", r.AppendInstructions))
	}
	if report != nil {
		args = append(args, "--output-schema", report.schema, "--output-last-message", report.output)
	}
	args = append(args, c.Args...)
	if c.Browser {
		args = append(args, nativecli.CodexBrowserArgs()...)
	}
	if c.Effort != "" {
		args = append(args, "-c", codexOverride("model_reasoning_effort", c.Effort))
	}
	args = append(args, "--")
	if r.ResumeSession != "" {
		args = append(args, r.ResumeSession)
	}
	return append(args, r.Prompt)
}

// codexOverride encodes a `-c` override as the TOML Codex parses. Invalid UTF-8
// becomes U+FFFD byte by byte, which is what the JSON encoding used here before
// did, so no value Args accepted before is refused now or means something else.
func codexOverride(key, value string) string {
	encoded, _ := restrict.TOMLString(string([]rune(value)))
	return key + "=" + encoded
}

// codexManagedOverrides are the config keys Codex's argv builder sets with -c.
var codexManagedOverrides = []string{"sandbox_mode", "developer_instructions", "model_reasoning_effort", "model"}

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
