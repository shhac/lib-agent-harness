package session

import harness "github.com/shhac/lib-agent-harness"

// ClaudeLinuxSandboxRequiresSocat is the named Linux sandbox prerequisite.
const ClaudeLinuxSandboxRequiresSocat = "claude_linux_sandbox_requires_socat"

// ClaudeLinuxInCommandLoopbackFailed names failure to reach a command's own listener.
const ClaudeLinuxInCommandLoopbackFailed = "claude_linux_in_command_loopback_failed"

// ClaudeLinuxLoopbackScopeWiderThanClaimed names host reach beyond the per-command contract.
const ClaudeLinuxLoopbackScopeWiderThanClaimed = "claude_linux_loopback_scope_wider_than_claimed"

// The disposable proof inherits the parent's PATH without changing it. Check
// the same resolution before starting a CLI or consulting cached proof evidence.
func checkClaudeSandboxPrerequisites(goos string, o Options, lookup func(string) (string, error)) error {
	if goos != "linux" || o.Provider.Engine != harness.Claude || o.Sandbox == nil {
		return nil
	}
	if _, err := lookup("socat"); err != nil {
		return &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch, Reason: ClaudeLinuxSandboxRequiresSocat}
	}
	return nil
}
