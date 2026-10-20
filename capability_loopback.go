package harness

// loopbackLocalOnlySupport is unsupported everywhere a session could ask for
// it; the reason says what is unproved for this engine and platform.
func loopbackLocalOnlySupport(goos string, e Engine, op Operation) Capability {
	if e == Claude && op == Session {
		if goos == "darwin" {
			return Capability{Unsupported, LoopbackLocalOnlySeatbeltReason + "; Claude allowLocalBinding uses *:* and local-only binds are unproved"}
		}
		if goos == "linux" {
			return Capability{Unsupported, "Claude Linux session local-only binds and inbound confinement have not been proved; standalone command namespace evidence does not establish this native-session capability"}
		}
		return Capability{Unsupported, "Claude allowLocalBinding permits binds on any interface; local-only binds are unproved on this platform"}
	}
	if goos == "darwin" && op == Session && e == OpenAICompatible {
		return Capability{Unsupported, LoopbackLocalOnlySeatbeltReason}
	}
	if goos == "linux" && e == OpenAICompatible && op == Session {
		return Capability{Unsupported, "workbench sessions have no shared private loopback; standalone sandbox.Open offers LoopbackLocalOnly after proving private-namespace loopback"}
	}
	return Capability{Unsupported, "local-only loopback is not proved for this engine, operation and platform"}
}

// LoopbackLocalOnlySeatbeltReason names the macOS local-bind limitation.
const LoopbackLocalOnlySeatbeltReason = "loopback_local_only_unenforceable: Seatbelt network filters accept only * or localhost hosts, and localhost admits binds and inbound connections on every local interface (owner-run evidence); local-only loopback cannot be enforced on macOS"

// Interface exposure is platform evidence, distinct from the per-launch proof.
const claudeLoopbackExposureReason = "macOS Claude Loopback is proved at launch by localhost reach and bind plus outbound off-machine refusal; LAH-39 real-Seatbelt evidence shows allowLocalBinding (local ip *:*) admits binds and inbound connections on every local interface, so 0.0.0.0 and LAN listeners may be reachable from other machines; no local-only claim; interface-address TCP bind, UDP bind and UDP send diagnostics record observations or unavailable measurements without gating the base proof"

// ClaudeLinuxHostLoopbackReason names requests outside the owner-observed
// per-command contract, including selected host-port requests.
const ClaudeLinuxHostLoopbackReason = "claude_linux_host_loopback_unavailable: Claude Linux sessions offer per-command loopback; the host's and other commands' servers are outside this contract"

const claudeLinuxLoopbackReason = "Claude Linux offers per-command loopback: a command can reach servers it starts itself; the host's and other commands' servers are outside this contract; owner-observed Ubuntu 24.04.5 / bubblewrap 0.9.0 / Claude Code 2.1.289 proved in-command reach and bind with host localhost unreachable and outbound off-machine traffic refused; proved before launch, host-shared scope refused; host-interface bind confinement remains unproved, no local-only claim; socat is required"

// LoopbackPortsSeatbeltReason names the unproved macOS selected-port boundary.
const LoopbackPortsSeatbeltReason = "loopback_ports_unenforceable: Seatbelt per-port rules can name only localhost, which admits TCP/UDP binds on every local interface (owner-run Darwin 27 evidence, LAH-39); selected ports cannot be confined to loopback on macOS; " + loopbackPortsAlternative

const loopbackPortsAlternative = "use Loopback via a separate sandbox.Open command sandbox on macOS or Linux for servers started inside the command; command sandboxing is unavailable on Windows and other platforms"

const loopbackPortsPrivateNamespaceReason = "private per-command loopback has no per-port filter; " + loopbackPortsAlternative

func loopbackPortsSupport(e Engine, op Operation, goos string) Capability {
	reason := "selected loopback ports are not proved for this engine, operation and platform"
	if op == Run {
		reason = "native runs have no proved per-port sandbox"
	} else if op == Session {
		switch e {
		case OpenAICompatible:
			switch goos {
			case "darwin":
				return Capability{Unsupported, LoopbackPortsSeatbeltReason}
			case "linux":
				return Capability{Unsupported, loopbackPortsPrivateNamespaceReason}
			default:
				reason = "selected loopback ports require proved OS enforcement"
			}
		case Claude:
			switch goos {
			case "linux":
				reason = ClaudeLinuxHostLoopbackReason
			default:
				reason = "Claude Code offers only yes/no allowLocalBinding"
			}
		case Codex:
			reason = "Codex native loopback is unsupported; no per-port enforcement proof"
		case Grok:
			reason = "Grok sessions have no proven OS sandbox"
		case CommandCode:
			reason = "Command Code sessions have no proven OS sandbox"
		case Antigravity:
			reason = "Antigravity sessions have no proven OS sandbox"
		}
	}
	if goos == "darwin" && op == Session {
		// Preserve each engine's limitation while explaining the common macOS
		// refusal code. No native engine gains a Seatbelt enforcement claim.
		return Capability{Unsupported, reason + "; " + LoopbackPortsSeatbeltReason}
	}
	return Capability{Unsupported, reason + "; " + loopbackPortsAlternative}
}
