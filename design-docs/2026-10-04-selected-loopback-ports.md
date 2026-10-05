# Selected loopback ports: public request, pre-launch refusal

## Decision (LAH-40, part A)

Re-land the public surface of LAH-24 (`4b7e9d6`, reverted by `b288d4d`)
by hand on the current tree. No platform offers selected-port confinement.
The owner clarified that LAH-24's Linux PASS covered refusal tests: private
per-command network namespaces cannot reach host ports and have no per-port
filter. Keep that behavior. No Linux host-port bridge is queued.

LAH-39 has landed. Its audit found no Seatbelt rule form that proves binds
confined to loopback. On the owner's Darwin 27 arm64 run, `localhost:<port>`
rules allowed all four loopback bind/reach controls and denied unselected
ports, but allowed TCP and UDP binds on non-loopback interface addresses.
Some UDP sends to those addresses also succeeded; errno 65 on other sends
does not negate the bind escapes. Literal IP hosts are rejected by Seatbelt.
This is attributed owner evidence, not a new enforcement proof.

## Public contract

`harness.LoopbackPorts` is Feature `"loopback_ports"`. `LoopbackPorts []int`
and `LoopbackControl string` are fields of `sandbox.Options`,
`session.CommandSandboxOptions` (deprecated), `session.Commands` and native
`session.Sandbox`. All engines and operations report Unsupported, never Unknown.

| Request | Platform | Capability reason |
| --- | --- | --- |
| Standalone commands / API workbench | macOS | `harness.LoopbackPortsSeatbeltReason`: `loopback_ports_unenforceable`; Seatbelt cannot confine TCP/UDP binds to loopback |
| Standalone commands / API workbench | Linux | `private per-command loopback has no per-port filter` |
| Standalone commands / API workbench | Windows / other | Selected ports require proved OS enforcement |
| Claude session | macOS | Only yes/no allowLocalBinding, plus the Seatbelt reason |
| Claude session | Linux | `harness.ClaudeLinuxHostLoopbackReason`: `claude_linux_host_loopback_unavailable` |
| Claude session | Windows / other | Only yes/no allowLocalBinding |
| Codex session | macOS | Native loopback unsupported; no per-port enforcement proof, plus the Seatbelt reason |
| Codex session | Other platforms | Native loopback unsupported; no per-port enforcement proof |
| Grok / Command Code session | macOS | No proven OS sandbox, plus the Seatbelt reason |
| Grok / Command Code session | Other platforms | No proven OS sandbox |
| Native Run | All | No proved per-port sandbox |
| Any remaining engine / operation | All | Selected ports not proved for that engine, operation and platform; macOS Session also includes the Seatbelt reason |

Every reason points to `Loopback` via a separate `sandbox.Open` command
sandbox on macOS or Linux for servers started inside the command, and states
that command sandboxing is unavailable on Windows and other platforms.
This names a working route for callers whose native engine has no Loopback
support; it does not claim native Codex/Grok/Command Code or Windows support.
Every macOS Session reason includes `LoopbackPortsSeatbeltReason` alongside
the engine limitation, matching the `loopback_ports_unenforceable` code.
That common platform limitation does not imply any native engine has a
proved Seatbelt sandbox.
On macOS plain Loopback admits all-interface binds/inbound with on-machine
outbound. Linux commands have private per-command loopback; a command must
start and request its own server, and cannot reach host or sibling servers.
Linux standalone Start still refuses exposing a loopback server to the host.

Validation is pure and ordered identically on every platform:

1. A non-nil list must contain 1–32 entries, each in 1–65535. Violations
   return `limit_exceeded`, even if Loopback is false or control is invalid.
2. Ports require Loopback. A control requires non-nil ports and must be an
   unscoped IP literal, excluding loopback (including mapped IPv4), unspecified,
   multicast and link-local addresses. Violations return `conflicting_options`.
3. Clone, sort and deduplicate the valid list; canonicalize the control literal.
   Refuse with `loopback_ports_unenforceable` on macOS or `not_offered` elsewhere.

No interface or resolver discovery is performed. A syntactically valid control
is still refused; whether that literal belongs to this machine is irrelevant
until an enforcement proof is offered. The test-only LAH-43 canary owns control
discovery and its off-machine witness. No caller-controlled hostname is resolved.

The clone does not share the caller's backing array or global state. Mutations
after normalization cannot change the evaluated list. As with any Go slice,
callers must not mutate their input concurrently with a call that reads it.
Concurrent calls with stable inputs have independent normalized copies.

## Refusal and compatibility boundaries

The network check runs before filesystem normalization, workspace/runtime
writes, login preparation, credential copies, probes, caches and launch.
Start, Resume, VerifySandbox and deprecated OpenCommandSandbox preserve the
sandbox refusal code through session's existing error translation. Session
reasons identify the actual engine. Direct workbench proof also checks the
request before entering the proof bridge. Errors retain Unsupported availability
and locate the separate sandbox.Open Loopback alternative on supported
platforms; nothing needs cleanup after refusal.

No proof, reference or cache entry is produced for selected ports. With nil
ports and an empty control, existing profiles, proof-key payloads and resume
digests remain byte-identical. Part B restores wrappers only for non-nil ports,
separating evidence by ports, control and a versioned per-port template. The
runner freezes a ports clone and refuses mutated requests before preparing state.
Callers using keyed error literals need no migration. Adding private control
metadata to the exported `sandbox.ProofError` breaks external unkeyed literals;
use keyed literals instead. A release containing this change must identify that
source-compatibility break. This task makes no consumer-app updates, release tag
or publication.

## Verification and follow-up

Tests cover the complete capability matrix, validation order, cloned/sorted/
deduplicated input, concurrent normalization and unchanged nil-port digest.
The existing pinned macOS profile/proof-key and Linux proof-key tests now
apply nil-port network normalization before comparing against their frozen
oracles. The macOS proof-key test also checks equality with the independently
pinned current v12 payload, not only inequality with earlier proof revisions.
Session tests require code/reason agreement and the concrete command alternative
for every engine through Start, Resume and VerifySandbox, and for the command
wrapper. Public paths refuse with an empty workspace/runtime and no fake
harness invocation or login files.
`TestSelectedPortRealSeatbelt` and `TestWorkbenchSelectedPortsRealProof` now
expect the named macOS refusal without prerequisites and never skip, including
under `AGENT_HARNESS_TEST_NO_SKIP=1` and inside sandboxcheck.

LAH-43 (part B) retains the selected-port Stage A/B canary from `4b7e9d6` in a
Darwin `sandbox/loopback_ports_canary_darwin_test.go` file, rather than a
production `loopback_ports_darwin.go`. Production normalization refuses before discovery; the
production proof entry supplies no selected-port stage and also refuses non-nil
ports. Tests inject the retained stage directly. There are no new exported API
members or `harness.Support` changes; network-control metadata stays private,
with the error-literal source-compatibility caveat above.

Stage A checks disposable selected-port bind/reach and host inbound receipts,
unselected ports including 8340, a proved off-machine DNS control and TCP witness.
Stage B keeps the exact requested list and never contacts an occupied selected
port. The socket helpers retain their `xcode-select` guard. Interface and wildcard
binds, UDP binds and sends run separately through shared `InterfaceAttemptsAtPort`,
`interfaceCanary` and `judgeInterfaceAttempts(interfaceLoopbackOnly)`.
An `interfaceAllLocal` parse preserves observations even when the restriction
judge returns an escape with no observations. TCP interface connects retain
their host witness and target only concrete interface addresses. Unspecified
destinations (`0.0.0.0` and `::`) are omitted from TCP reach attempts because the
kernel redirects them to permitted loopback, which would falsely report escape.
Wildcard bind attempts remain in the shared interface stage. Shared Perl/Python
clients and pinned bytes are unchanged.

`proveWorkbenchStages` clears ports/control for the base proof, serializes only
identical keys, then publishes network evidence once after both stages settle
and the context remains live. Control metadata is private on `networkEvidence`
and `ProofError`, never in `HarnessFacts`. Cancellation gives `probe_timed_out`;
a missing control gives `control_deadline` or `no_off_machine_resolver`, and
missing developer tools/Python readiness gives `port_client_unavailable`.
No failure publishes evidence; listeners and disposable scratch settle through
defers. Cache entries are process-local only, and reset clears network evidence
as well as keys. Selected ports and controls cannot share evidence.

macOS CI pins `AGENT_HARNESS_TEST_LOOPBACK_CONTROL` using
`go run ./internal/cmd/loopbackcontrol`: it proves a configured resolver with a
matching UDP DNS reply and TCP connection, with no fixed public fallback.
`TestSelectedPortCanaryStillEscapes` uses real Seatbelt and requires
`sandbox_not_enforced / selected_port_network` with interface observations.
A successful canary or a different failure produces a **re-audit** failure.
Missing prerequisites may skip only outside `AGENT_HARNESS_TEST_NO_SKIP=1`.
The existing public refusal test remains unconditional.

A future enablement requires
positive selected-port availability and denial of unselected ports, interface
binds, interface UDP sends and off-machine traffic; it must first pass real
installed-runtime proof. No refusal can be promoted by skips or fixture echoes.

The task note identifies the exact draft revision for the owner's unsandboxed
macOS and Ubuntu 24.04 / bubblewrap 0.9.0 runs:
`AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -count=1 ./...` and
`go run ./internal/cmd/sandboxcheck --no-skip`, with the macOS control pinned as
above. Exit codes and verbatim canary/escape outcomes remain distinct runtime
evidence from CI and do not replace team-runnable checks.

Validation commands: `go vet ./...`, `go test -race ./...`, CGO-free Linux and
Windows vet/test cross-compilation, and the project's daemon check. CI runs
strict race tests on macOS, Windows and both Linux bubblewrap jobs (0.9 and
0.8); macOS and Linux also run sandboxcheck. Unsandboxed owner strict-suite
and sandboxcheck runs on real macOS and Ubuntu remain distinct runtime evidence.
