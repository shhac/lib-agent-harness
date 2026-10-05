# Selected loopback port requests (unreleased; source compatibility note)

Adds Feature `harness.LoopbackPorts` (`"loopback_ports"`) and fields
`LoopbackPorts []int` / `LoopbackControl string` on `sandbox.Options`,
`session.Commands`, native `session.Sandbox` and deprecated
`session.CommandSandboxOptions`. No platform offers selected ports.

macOS refuses before discovery, state writes, credential preparation or launch
with `loopback_ports_unenforceable` (exported in sandbox and session as
`RefusedLoopbackPortsUnenforceable`) and `harness.LoopbackPortsSeatbeltReason`.
Every macOS Session reason includes this Seatbelt reason alongside its
engine limitation, including Codex, Grok and Command Code; this does not
claim those engines have a proved Seatbelt sandbox.
LAH-39 and the owner's Darwin 27 evidence show Seatbelt localhost port rules
permit TCP/UDP binds on every local interface. Linux retains LAH-24's refusal:
private per-command loopback has no per-port filter. Windows is also unsupported.
Linux and Windows return `not_offered`. Claude Linux's capability reason names
`claude_linux_host_loopback_unavailable`; native runs have no per-port proof.

Every refusal locates the nearest alternative: plain Loopback via a separate
`sandbox.Open` command sandbox on macOS or Linux for servers started inside
the command. Errors state that command sandboxing is unavailable on Windows
and other platforms; no native-engine Loopback support is implied. Plain macOS
Loopback still exposes all-interface binds/inbound; Linux loopback stays private to each
command and cannot reach host ports. No enforcement or existing Loopback behavior
changes. Invalid requests have deterministic limit/conflict errors everywhere.

Nil-port profiles, proof keys and resume digests are unchanged. No consumer
migration for keyed error literals, consumer-app change, release tag or publication
is required here.
LAH-43 restores the test-only selected-port canary
and interface escape attempts; it cannot enable production support.
The retained macOS escape canary adds no exported API. Private control metadata
fields added to `sandbox.ProofError` do change source compatibility: external
unkeyed literals no longer compile. Use keyed literals such as
`&sandbox.ProofError{Code: code, Step: step}`. Any release containing this change
must identify that source break; no release is tagged or published by this task.
See the [design record](../design-docs/2026-10-04-selected-loopback-ports.md)
for the complete reason matrix, validation order and proof requirements.

CLI transport failures now preserve the caller's context error when cancellation
races child termination, including account inspection. Successful replies remain
successful; cancellation still settles the child before inspection returns.
