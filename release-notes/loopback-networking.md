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
Selected-port restrictions are tracked by LAH-40; after-landing native experiments stay
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
LAH-40 must retain the macOS selected-port refusal; LAH-41 extends Claude
interface canaries. No release tag or consumer-app change is made here.

The macOS interface client uses base-system /usr/bin/perl; it does not invoke
the Xcode python3 shim or require developer tools. Interface witnesses try
remaining addresses when one cannot bind. Unavailable interface binds are
reported as such, never as observed exposure. A changed Seatbelt bind contract
refuses proof with `loopback_interface_claim_changed`. Cache success and
observations publish atomically; only identical in-flight keys queue together.
Existing Linux failure classification is unchanged unless LocalOnly is requested.

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
