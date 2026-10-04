# Selected localhost ports

The optional network policy extends the existing command sandbox. It adds no
filtering subsystem and does not change native loopback findings in
[loopback networking](2026-10-04-loopback-networking.md). It builds on the landed
readable-PATH, read/execute and execution-proof revisions, and preserves the
current file-tool refusals. Session read profiles and provider-egress work follow
this network change; storage/admission work remains independent.

## Public policy and support

`sandbox.Options`, `session.Commands` and the deprecated
`session.CommandSandboxOptions` carry `LoopbackPorts []int` and
`LoopbackControl string`. Native `session.Sandbox` carries `LoopbackPorts` for
an explicit pre-login refusal. No breaking API change or consumer migration is
required. Tagging, publishing and consumer rollout are owner steps after landing.

| Surface / engine | macOS | Linux | Windows / other |
| --- | --- | --- | --- |
| Standalone commands / compatibility wrapper | Seatbelt proof required; failed proof refuses | Unsupported | Unsupported |
| API workbench commands, OpenAI-compatible Session | `LoopbackPorts`: Unknown until installed proof | Unsupported: private namespace lacks per-port filtering | Unsupported |
| Claude native Session | Unsupported: `allowLocalBinding` is yes/no | Unsupported | Unsupported |
| Codex native Session | Unsupported: native loopback remains unproved | Unsupported | Unsupported |
| Grok / Command Code Session | Unsupported: no proved OS sandbox | Unsupported | Unsupported |
| Run, every engine | Unsupported | Unsupported | Unsupported |

Linux commands keep their existing private per-command loopback. They cannot
share a host-visible server with another command. Neither host-network
forwarding, privileged firewall changes nor a new socket filter is in scope.
Windows refuses before launch. The list does not apply to hosted caller tools,
browser channels or Unix-domain sockets. Native session loopback is unchanged.

A nil list leaves normalization, Seatbelt bytes, verification identity and
references unchanged. A non-nil list requires Loopback; empty is refused, never
interpreted as all ports. Validate 1–32 input entries, each in 1–65535; clone,
sort and deduplicate before proof. Reject invalid policy before system discovery
or state creation, including on unsupported platforms. A control without a list
is a conflict. The control must be an IP literal off this machine: reject local
interface, loopback (including mapped IPv4), unspecified, multicast, link-local
and scoped addresses. Private off-machine addresses remain valid.

## Outside controls and structural facts

Discovery selects the caller's control first; it never falls back if that
control fails. Configured candidates are tried in order, with the remaining
three-second budget divided among the remaining candidates. A failed IPv6
control can therefore fall back to another configured server. Otherwise macOS parses `scutil --dns` nameserver entries,
including scoped resolver sections and IPv6. The Linux parser reads
`/etc/resolv.conf`; when it contains only local stubs, it reads upstream servers
from `/run/systemd/resolve/resolv.conf`. Linux has no enforcement path using
these controls while selected ports remain unsupported. There is no default
public resolver. Discovery and both port-53 controls share a three-second
deadline. The unchanged TCP 443 witness also has its existing outside control.

The DNS control sends a root NS query with a random transaction ID and no caller
data over UDP 53. A response must contain a DNS response header with the same
ID. TCP 53 must accept a connection. Successful proof records only the destination
IP and source (`caller`, `configured`, or `systemd_upstream`), exposed by
`Proof.NetworkControl()` and `Session.NetworkControl()` for API workbenches.
After selection, failed proofs also carry `ProofError.ControlAddr/ControlSource`;
compatibility translation preserves them in `CapabilityError`. Both expose
canonical IP/source through `Facts.NetworkControlAddr/NetworkControlSource`.
Untrusted addresses and source labels are stripped. Query and reply bytes never
enter errors or facts. Shared exported `sandbox.ProofStep*` constants define
the fixed reason vocabulary used by both error surfaces.

Unavailable reasons are fixed `ProofError.Step` / `Facts.ProofStep` values:
`no_off_machine_resolver`, `control_on_this_machine`, `control_invalid_ip`,
`control_interface_unavailable`, `udp_unanswered`, `tcp_unanswered`,
`control_deadline`, `port_client_unavailable`, `ipv6_control_unavailable`, or
`selected_port_network`. Session compatibility translation preserves these.
Invalid caller controls are normalization refusals; unsuccessful outside
controls are unavailable, not enforcement evidence.

## Two network stages on the existing boundary

The unchanged filesystem/execution, supervisor, recovery and ordinary-loopback
proof runs first with disposable state. It includes the landed readable-PATH
and native-executable controls. A selected-policy cache entry is published only
after the additional network proof succeeds. No workspace tools or durable
command state are admitted between these proofs.

Stage A renders the same Seatbelt generator with two probe-selected free ports.
Each gets per-port local bind/inbound and remote outbound selectors, replacing
`localhost:*`. The socket helper proves bind and a data round trip on
127.0.0.1, ::1, ::ffff:127.0.0.1 and localhost. Independent IPv4 and IPv6 host
listeners prove reach. A separate host client sends an inbound nonce and reads
the sandbox server's nonce reply. Separating these runs prevents that client
from consuming the helper's internal positive-control connection.

Excluded ports, including 8340, must immediately refuse TCP bind/reach and UDP
bind/send. Wildcard IPv4/IPv6 and available interface addresses, including scoped
IPv6 link-local addresses, must also refuse those operations on a selected port.
The stage checks off-machine TCP 443 and TCP/UDP 53 too. A successful operation
is an escape, even if a UDP send receives no reply. Independent host IPv4/IPv6
listeners on the excluded probe port positively witness admitted connects.
Interface/wildcard reach runs separately after the inside server has settled,
with host wildcard listeners on the selected port. This prevents a missing
listener from masking an admitted connect as ECONNREFUSED.

Stage B renders the caller's exact frozen list with its workspace/read selectors
and disposable private scratch. It checks a probe-selected excluded port, 8340
when excluded, and off-machine TCP 443 / TCP 53 / UDP 53. It proves bind and
host-to-sandbox receipt on a free selected port, then reach with a host listener
after that server has settled. Busy selected ports are never contacted; Stage A
proves the generator's positive rules. A port becoming busy during the probe
refuses incomplete evidence; a port becoming busy at command launch produces
an ordinary command bind failure without permission widening.

The installed `/usr/bin/python3` socket client reports errno directly, under the
same read/execute confinement as commands. Before invoking Apple's Python shim,
`xcode-select -p` must identify an installed developer tree, preventing the
missing-tools installation dialog. Its outside import control must work;
no interpreter dependency is added to the read allow-list. EPERM or EACCES is
denial. Bind/listen failures report their phase and errno immediately; socket
operations and helper steps have short deadlines, including inbound receipt.
ECONNREFUSED, timeout, silence, missing client/runtime, partial supervisor
output, missing completion or missing nonce is unavailable. Any successful
forbidden operation is `sandbox_not_enforced`. No failed proof becomes a skip
or a successful cache entry. Cancellation during sandbox stages reports
`probe_timed_out`; discovery/control deadlines report `control_deadline`.

## Evidence and reference identity

Absent ports preserve the legacy key payload and template version byte for
byte. Selected-port keys wrap that payload with frozen ports, explicit control
and a separate port-template/probe-revision hash (selected-ports-v2). Concurrent different lists or
controls cannot share evidence. The discovered resolver is evidence rather than
permission and is not in the key. The bounded, in-memory cache stores the
successful control facts with the proof atomically; restart re-proves, possibly
with a different resolver. No persistent proof is introduced.

`NewRunner` compares both original-request and normalized options against the
proof and launches only its frozen options. Original slices are cloned too.
Different ports or controls fail with `state_unusable` before state preparation.
Workbench references wrap the existing permission digest only when ports are
supplied. A changed, added or removed list cannot resume. Control-only changes
preserve references but use a different verification key. Recovery markers and
unknown-outcome semantics are unchanged; interrupted commands are never replayed.

## Verification record

See the final test record below for commands actually run and their environment.
Synthetic policy, parser, DNS-control, judge, key, reference, recovery and runner
tests use disposable state, fake transports and no models or account credentials.
Real Seatbelt tests use the existing nested-sandbox and loopback prerequisite
helpers. Socket mechanics tests first attempt an explicit-port bind from Go;
only a proved environment permission refusal can skip that prerequisite, with
a named reason. `AGENT_HARNESS_TEST_NO_SKIP=1` fails prerequisite refusals.
Timeouts always fail. Once a real proof runs, every proof error fails the test,
including NotEnforced, Unavailable and ProbeTimeout; none logs-and-passes. A successful test records
both stages and its DNS destination/source. macOS CI pins a responding configured resolver with
`AGENT_HARNESS_TEST_LOOPBACK_CONTROL` using the bounded discovery/control helper;
failure to find one fails CI. Tests never choose a public default.

Cross-compilation is compile evidence, not Linux/Windows execution or race
evidence. The existing strict CI matrix remains required on macOS, Linux and
Windows. No publishing or owner machine is needed for the daemon project check.

### Draft 1 commands actually run, 2026-10-04 (historical)

Local environment: Go 1.27.1, Darwin/arm64, inside the task sandbox.

| Check | Result / limit |
| --- | --- |
| `go vet ./...` | Passed on draft 1 |
| `go test -race ./...` | Passed on draft 1; environment prerequisite skips remain enabled |
| Focused selected-port, DNS-control, judge, key, recovery and proof-publication race tests | Passed |
| `go test -v ./sandbox ./session -run 'SelectedPort\|PortPolicy'` | Synthetic tests passed; both new real Seatbelt tests skipped at the independent nested-sandbox prerequisite, exit 71 / permission denied |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...` | Passed |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...` | Passed |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | All test packages compiled; Linux binaries were not executed |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | All test packages compiled; Windows binaries were not executed |
| Daemon `run_check`, polled until completion | Draft 1 `go vet ./...` / `go test -race ./...` passed, exit 0, no timeout; sandbox package 8.366s, session package 128.231s |

Earlier daemon runs exposed a synthetic recovery fixture's dependency on real
interface enumeration and a TIME_WAIT collision in repeated socket-helper bind
controls. The fixture now injects its known control inventory at both internal
normalization boundaries; production guards remain intact. Socket helpers set
SO_REUSEADDR, so repeated disposable controls and the separate inbound server
can rebind after settlement. Both failures were corrected and the project check
rerun successfully. One intervening check failed to copy a changing temporary
directory while local tests were running; the final check started after local
test cleanup settled.

No real selected-port Seatbelt Stage A/B success or off-machine TCP/UDP 53
enforcement was observed here. The environment refuses nested Seatbelt; network
access is unavailable. The successful daemon run validates tests and explicit
refusal behavior, not installed enforcement. macOS support therefore remains
Unknown until each installed profile passes its complete pre-launch proof.
Native engines and Linux/Windows retain Unsupported. Linux/Windows runtime
race results and the complete strict CI matrix remain unobserved from this
Darwin sandbox; cross-compilation is not a substitute. Real macOS canary
evidence and platform CI outcomes must be recorded against the landed candidate.


### Review repair verification, 2026-10-04

The review repair makes both real Seatbelt tests fatal on any proof failure and
exposes successful API-session and failed-proof control metadata. Synthetic
helper failures and the local socket-mechanics test use a PATH Python interpreter
outside Seatbelt to test status reporting and socket operations. This cannot prove Apple's shim or read/execute
profile; only the strict real proof tests can do that. An initial synthetic test
incorrectly depended on the shim and failed its bounded readiness check here;
it was corrected without treating the timeout as a skip. The first repaired
daemon run exposed the same shim startup timeout in the outside socket-mechanics
test (no helper status was emitted); it now uses the test interpreter too, with
all selected-port socket assertions retained. Both real proof tests continue to
require Apple's interpreter inside the actual command profile.

Linux/Windows CGO-free vet and test cross-compilation passed after the repairs.
| Repair check | Result / limit |
| --- | --- |
| Local `go vet ./...` and `go test -race ./...` | Passed; final sandbox 27.071s, session 50.479s |
| `go test -v ./sandbox ./session -run '^(TestSelectedPortRealSeatbelt\|TestWorkbenchSelectedPortsRealProof)$'` | Both skipped only at nested Seatbelt prerequisite: sandbox-exec permission denied, exit 71; no proof stages ran |
| Focused race tests for policy, control selection, helper failures, judgments, keys, recovery, cancellation and control facts | Passed |
| Helper bind/listen failure and missing-developer-tools tests with `AGENT_HARNESS_TEST_NO_SKIP=1` | Passed; synthetic outside-sandbox tests, not enforcement evidence |
| `GOOS=linux/windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...` | Both passed on repaired code |
| `GOOS=linux/windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | Both compiled all test packages; foreign binaries were not executed |
| First repaired daemon `run_check` | Failed: socket-mechanics test shim startup timed out before emitting status; corrected as described above |
| Final repaired daemon `run_check`, polled to completion | Passed vet/race, exit 0, no timeout; sandbox 9.011s, session 120.848s |

No new real Stage A/B enforcement evidence is claimed: the task environment
refuses the independently checked nested Seatbelt prerequisite. The owner runs
the strict suite and sandboxcheck outside this sandbox on the landed candidate
and records real macOS enforcement and Linux/Windows CI outcomes.
