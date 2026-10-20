package native

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ClaudeOptions are Claude's own execution options.
type ClaudeOptions struct {
	PermissionMode string
	// AllowedTools are permission allow rules. Empty adds none; it does not
	// remove tools.
	AllowedTools []string
	// MaxBudgetUSD caps the API-rate valuation of one invocation; zero is no cap.
	MaxBudgetUSD float64
}

func (o ClaudeOptions) set() bool {
	return o.PermissionMode != "" || o.AllowedTools != nil || o.MaxBudgetUSD != 0
}

func claudeArgs(c Config, r Request) []string {
	args := []string{"-p"}
	if r.ResumeSession != "" {
		args = append(args, "--resume", r.ResumeSession)
	}
	args = append(args, "--output-format", "stream-json", "--verbose")
	if r.Schema != "" {
		args = append(args, "--json-schema", jsonCompact(r.Schema))
	}
	if r.AppendInstructions != "" {
		args = append(args, "--append-system-prompt", r.AppendInstructions)
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if c.Effort != "" {
		args = append(args, "--effort", c.Effort)
	}
	if c.Claude.PermissionMode != "" {
		args = append(args, "--permission-mode", c.Claude.PermissionMode)
	}
	if c.Browser {
		args = append(args, "--chrome")
	}
	if len(c.Claude.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(c.Claude.AllowedTools, ","))
	}
	if c.Claude.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(c.Claude.MaxBudgetUSD, 'f', -1, 64))
	}
	args = append(args, c.Args...)
	return append(args, "--", r.Prompt)
}

func isDefaultClaudeHome(home string, env []string) bool {
	userHome := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "HOME=") {
			userHome = strings.TrimPrefix(entry, "HOME=")
		}
	}
	if userHome == "" {
		userHome, _ = os.UserHomeDir()
	}
	return userHome != "" && filepath.Clean(home) == filepath.Join(userHome, ".claude")
}
