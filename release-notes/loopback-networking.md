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
Selected-port restrictions remain LAH-24; after-landing native experiments stay
in the owner's checklist.
