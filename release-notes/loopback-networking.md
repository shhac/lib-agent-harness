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
After-landing native experiments stay in the owner's checklist.

## Selected command-loopback ports (unreleased, additive)

`sandbox.Options`, API `session.Commands` and the retained deprecated command
wrapper accept optional `LoopbackPorts` and `LoopbackControl`. Nil preserves
existing profiles, verification keys and references. A supplied list is frozen,
validated and enforced only after a complete macOS Seatbelt proof; an empty list
never means unrestricted networking. Native `session.Sandbox.LoopbackPorts`
explicitly refuses before login preparation.

| Engine / surface | macOS | Linux | Windows / other |
| --- | --- | --- | --- |
| Standalone and deprecated command sandbox | Proof required; explicit refusal on failure | Unsupported | Unsupported |
| OpenAI-compatible Session workbench commands | Unknown until Seatbelt proof | Unsupported: private per-command loopback | Unsupported |
| Claude Session | Unsupported: yes/no local binding | Unsupported | Unsupported |
| Codex Session | Unsupported: native loopback unproved | Unsupported | Unsupported |
| Grok / Command Code Session | Unsupported: no proved OS sandbox | Unsupported | Unsupported |
| Every Run engine | Unsupported | Unsupported | Unsupported |

Proof includes selected bind/reach/inbound receipt, excluded ports and 8340,
address forms and off-machine TCP 443 plus TCP/UDP 53 with immediate permission
denial. Port-53 controls use a caller-selected IP or the configured resolver,
never a fixed public resolver, and fail within a short deadline if unavailable.
Successful proof exposes the control IP/source without DNS contents. The
installed socket helper must work under the unchanged read/execute profile.

Workbench resume identity includes ports but excludes the control destination;
changing only the control requests fresh evidence. Linux's private loopback,
readable-PATH filtering, recovery and disabled file-tool surface are unchanged.
Hosted tools, browser channels and Unix sockets are outside this policy.

Example: `session.Commands{Loopback: true, LoopbackPorts: []int{3000, 8080}}`.
See the [design and verification record](../design-docs/2026-10-04-selected-loopback-ports.md).
No breaking API change or coordinated consumer migration is necessary. Tagging,
publishing and consumer rollout remain owner steps after landing.

Review repairs make real Seatbelt and API-workbench proof failures fail tests,
including observed escapes; only independent permission-refused prerequisites
may skip, and strict CI forbids those skips. macOS CI pins a responding configured
resolver. Configured controls try subsequent candidates within the same deadline;
caller controls never fall back. Successful sessions expose the control through
`Session.NetworkControl()`; failed proofs preserve its IP/source in structural
facts and compatibility errors. Developer-tool checks prevent Python installation
UI, and bounded helper failures immediately report their phase/errno. Host
listeners positively witness excluded and interface-address connect escapes.
