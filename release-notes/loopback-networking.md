# Native Codex loopback refusal (unreleased)

Codex session loopback remains unsupported. Its capability diagnostic now names
Codex 0.160.0 and the owner's installed-runtime macOS observations: closed
networking refused loopback; enabled localhost-rule variants, including a proxy
variant and a real native session, allowed off-machine TCP 443 and TCP/UDP
port 53. No tested variant provided loopback-only enforcement. Windows retains
its sandbox refusal and includes the networking explanation in the capability
reason.

Regression coverage verifies refusal before login/runtime preparation in Start,
Resume and VerifySandbox. No public API, capability availability or consumer
configuration changes. Closed-network sessions and command-sandbox loopback
retain their existing behavior; no consumer migration is required.

The [research record](../design-docs/2026-10-04-loopback-networking.md) distinguishes
the four owner experiments from the team's refused sandbox prerequisite and
records missing app-server readback and untested platforms. The owner directs
retaining the refusal and states: "No further team experiment is needed."
No claim is made about every possible configuration or untested platforms.
Selected-port requests are refused everywhere by LAH-40 (see
[selected-port release notes](selected-loopback-ports.md)); native experiments stay
in the owner's checklist.


## macOS command loopback exposure (LAH-39, unreleased; non-breaking)

Existing Loopback callers remain offered on macOS, with corrected claims:
binds and inbound connections are allowed on every local interface, so a
server bound to 0.0.0.0 or a LAN address may be reachable from other machines.
Outbound traffic off the machine stays refused. This affects standalone
sandbox.Open (LAH-11), Workbench run_command and Claude allowLocalBinding
wording. Native Codex Loopback (LAH-19) remains Unsupported.

Callers can require `LoopbackLocalOnly` together with `Loopback` in
sandbox.Options, session.CommandSandboxOptions, session.Commands and
session.Sandbox. `harness.LoopbackLocalOnly` is Feature
`"loopback_local_only"`. macOS refuses it before probes or launch with
`loopback_local_only_unenforceable`: no tested Seatbelt rule form confines
binds to loopback. Linux offers the stricter request only for standalone
sandbox.Open, proved in each command private namespace. Session support
remains Unsupported. Missing Loopback gives `conflicting_options`.

The macOS canary records interface TCP bind, UDP bind and UDP-send errnos,
wildcards, and a host-side interface nonce connection.
`Proof.NetworkObservations()` and `Proof.NetworkDetail()` expose structural
evidence. seatbelt-workbench-v12 invalidates earlier unchecked proofs (one
re-proof on first Open). Existing option keys and resume digests are unchanged,
except this deliberate macOS proof-version bump; enabling LocalOnly changes
the Linux proof key and session power digest.

crew-assistant follow-up: check_loopback and CA-79 must show the all-interface
macOS wording and allow a per-project choice of local-only (refused on macOS)
or all-interfaces. Reference the field, Feature and refusal above.
LAH-40 retains the named macOS selected-port refusal; LAH-43 owns the kept test-only canary. No release tag or consumer-app change is made here.

The macOS interface client uses base-system /usr/bin/perl; it does not invoke
the Xcode python3 shim or require developer tools. Interface witnesses try
remaining addresses when one cannot bind. Unavailable interface binds are
reported as such, never as observed exposure. A changed Seatbelt bind contract
refuses proof with `loopback_interface_claim_changed`. Cache success and
observations publish atomically; only identical in-flight keys queue together.
Existing standalone Linux failure classification is unchanged unless LocalOnly is requested.

Owner evidence on Darwin 27 arm64, draft `6ffd2c6`, proves that local
ip/ip4/ip6/tcp/udp localhost:* forms permit TCP and/or UDP binds on private
IPv4, CGNAT, global IPv6, ULA, link-local and wildcard addresses. Family and
protocol selectors only narrow family or protocol. UDP sends to own interface
addresses succeed under every form; wildcard sends return errno 65. Literal
hosts are rejected with `host must be * or localhost in network address`.
See the design record for the rule-form table and historical lo0 control.
Tests report address classes without host addresses; loopback interfaces are
excluded from non-loopback escape evidence. The revised Perl-client proof
still requires a real unsandboxed run; the owner's earlier Python-client run
passed sandboxcheck and the interface proof but failed the now-corrected
literal-host assertion.

## Claude macOS interface diagnostics (unreleased; non-breaking)

Claude macOS Loopback remains offered after its existing base nc proof passes.
Support is Unknown until launch verification: localhost reach and bind must
succeed, and off-machine access must be refused. Start, Resume and
VerifySandbox retain that proof path. Strict LoopbackLocalOnly remains refused.

The all-interface exposure is stated from LAH-39 real-Seatbelt evidence:
allowLocalBinding admits binds and inbound connections on every local
interface, so wildcard and LAN listeners may be reachable externally. No
local-only claim is made. The discarded draft-only interface-canary refusal
is removed; no consumer migration is required.

Optional flat nc diagnostics record TCP source-bind, UDP source-bind and UDP
send host observations, or unavailable measurements, privately beside the
base verification. Unavailable diagnostics do not remove the capability.
Wildcard observations mean any peer, not a verified unspecified source bind.
No errno is inferred from exit status, and no permissions are widened.
The owner runner prints redacted per-class results without failing solely
because these optional measurements cannot run.

Shared helpers retain their aliases, Perl byte pin and workbench profile pin.
Linux canary bytes, Windows refusal and durable formats are unchanged. Linux
production admission now uses the per-command scope described below.
No consumer-app update or release tag is needed for this additive change.

## Claude Linux per-command Loopback

The owner's Ubuntu 24.04.5 / bubblewrap 0.9.0 / Claude Code 2.1.289 run
on `ce0e0d2001df0eef511af02ab954c71d408654ad` proved in-command loopback
with host localhost unreachable and off-machine traffic refused.
Claude Sandbox.Loopback remains offered on Linux as Unknown, proved for the
installed binary and launch configuration. A command can start and call its
own server; the host's and other commands' servers are outside the contract.
The unchanged nc canary now judges in-command success separately from host
scope, records per-command scope with settled verification evidence, and refuses
wider host-shared scope before launch.

Named refusals:

- `sandbox_unavailable` with `CapabilityError.Reason = session.ClaudeLinuxInCommandLoopbackFailed`
  when a completed canary cannot reach its own listener.
- `sandbox_not_enforced` with reason `session.ClaudeLinuxLoopbackScopeWiderThanClaimed`
  when a proof observes host-shared loopback.
- `harness.ClaudeLinuxHostLoopbackReason` exports
  `claude_linux_host_loopback_unavailable` for selected host-port requests;
  the additive LAH-40 API refuses LoopbackPorts before discovery or launch.

This is not breaking for in-command Loopback callers: the earlier production
host-reach rule could not pass the owner's per-command runtime. No host-reach
guarantee or bridge is introduced. Unsandboxed sessions, macOS behavior, Windows
refusal, durable formats and standalone/workbench proof keys are unchanged.

Every sandboxed Claude session on Linux (with or without Loopback) now checks
for socat before Start, Resume and VerifySandbox, including cached proofs.
Missing socat returns the existing `sandbox_unavailable` code with named reason
`claude_linux_sandbox_requires_socat` and installation guidance (`sudo apt install
socat`, `sudo dnf install socat`, or `sudo pacman -S socat`). This diagnostic
improvement is non-breaking: Claude's sandbox already required socat. Lookup
and the disposable proof use the library process's PATH; a session Env PATH
override must also permit the launched CLI to resolve socat.

The unrelated completion error-classification fixture allows 10 seconds for
process startup under race instrumentation; its actual timeout branch retains
the 300ms deadline. This is test-only stabilization.

CA-120 and CA-79 can describe the per-command contract and named refusals;
consumer adoption and publishing a release remain separate work.

The exported reasons above have stable values `claude_linux_in_command_loopback_failed`
and `claude_linux_loopback_scope_wider_than_claimed` for callers to match.

The owner's `0c1580c` run on Ubuntu 24.04.5 / bubblewrap 0.9.0 / Claude Code
2.1.289 / Python 3.12.3, with socat installed, confirmed production VerifySandbox
success and recorded per-command scope. The corrected double-quoted Python line
still did not establish dontAsk auto-approval: tool-result=true, socket-results=0,
canary-ran=false; every interface row unavailable. nc showed no host delivery.
Host-interface bind confinement remains unproved because the installed client
could not be auto-allowed, not because binds were denied. LoopbackLocalOnly stays
Unsupported. The owner waived the interface errno requirement and approved landing
on production evidence. The runner now reports optional measurements, launch
refusals and diagnostic host-delivery contradictions without failing; production
scope assertions and off-machine escape still fail. The owner will rerun this
report-only test after landing.
