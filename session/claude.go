package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Claude's session adapter: its launch arguments, policy and environment, and
// the stream-json conversation's start, turns and interruption. Its wire
// dialect, events, sandbox and telemetry have files of their own; its start-up
// tool-surface check stays shared (events.go).

// claudeHomeValue leaves CLAUDE_CONFIG_DIR unset for the CLI's own default
// home, where setting it would move Claude's project and credential lookup.
func claudeHomeValue(o Options) (string, bool) {
	home, err := os.UserHomeDir()
	if err == nil && filepath.Clean(o.Provider.CLI.Home) == filepath.Join(home, ".claude") {
		return "", false
	}
	return o.Provider.CLI.Home, true
}

// claudeOverrides turns off auto-memory in a sandboxed session: it would read
// and write the operator's own memory folders.
func claudeOverrides(o Options, env []string) []string {
	if o.Sandbox == nil {
		return env
	}
	return append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
}

// claudeArgs runs Claude Code's stream-json protocol for one conversation.
func claudeArgs(o Options, nativeID string, resuming bool, l *launch) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-mode", o.Policy.ClaudePermission}
	if l != nil {
		args = append(args, l.extra...)
	}
	if o.Browser {
		args = append(args, "--chrome")
	}
	if resuming {
		args = append(args, "--resume", nativeID)
	} else {
		args = append(args, "--session-id", nativeID)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.Policy.ClaudeTools != nil {
		args = append(args, "--tools="+strings.Join(o.Policy.ClaudeTools, ","))
	}
	args = append(args, claudeSkillArgs(o)...)
	if instructions := effectiveInstructions(o); instructions.Mode != "" {
		flag := "--system-prompt"
		if instructions.Mode == Append {
			flag = "--append-system-prompt"
		}
		args = append(args, flag, instructions.Text)
	}
	return args
}

func claudeRestrictedLaunch(_ context.Context, o Options, host *toolHost) (Options, []string, error) {
	return o, claudeRestrictedArgs(host), nil
}

func normalizeClaudePolicy(o Options) (Options, error) {
	o = legacyPolicyDefaults(o)
	switch o.Policy.ClaudePermission {
	case "dontAsk", "default", "acceptEdits", "plan", "auto":
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Claude permission policy")
	}
	return o, nil
}

// initializeClaude asks the CLI to initialize, which also reports the account.
func (s *Session) initializeClaude(ctx context.Context, _ bool) error {
	body, err := s.transport.request(ctx, "initialize", map[string]any{})
	if err != nil {
		return err
	}
	s.observeClaudeAccount(body)
	return nil
}

func (s *Session) startClaudeTurn(request, _ context.Context, _ *Turn, ref Ref, in Input) error {
	return s.transport.send(request, claudeUserFrame(ref.ID, in.Text))
}

func (s *Session) interruptClaude(ctx context.Context, _ string) error {
	_, err := s.transport.request(ctx, "interrupt", map[string]any{})
	return err
}
