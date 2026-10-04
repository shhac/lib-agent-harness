# Native Codex loopback: enforcement unproved

## Decision

`harness.Support(Codex, Session, Loopback)` remains `Unsupported` everywhere.
The refusal names Codex 0.160.0 and the owner's installed-runtime macOS results:
closed networking refused loopback; enabled localhost-rule variants and a real
native session allowed off-machine TCP 443 and TCP/UDP port 53. No tested variant
provided loopback-only enforcement. This does not assert that every possible
configuration is unsafe or extrapolate macOS behavior to Linux or Windows.
There is no new launch policy, successful loopback proof cache entry or loopback
resume format.
Start, Resume and VerifySandbox share normalization and refuse before private
login preparation or native execution. Windows retains its earlier pre-launch
sandbox-unavailable error; its capability reason also names the networking gap.

Existing closed-network sessions, their filesystem restrictions, native tools,
login reconciliation and browser confinement are unchanged. Command-sandbox
loopback is unchanged: macOS host-local access and Linux per-command private
loopback remain separate contracts. Selected ports belong to LAH-24;
read allow-lists and access requests belong to LAH-21/LAH-22. LAH-18 and LAH-23
are recorded as landed; no ordering decision or additional split is needed.

## Owner execution evidence, 2026-10-04

Source: LAH-19 owner note 6 (note id `b41204e8354306ca7e9b10eb`, posted
2026-10-04T07:20:32Z). These experiments were run by the owner **outside any
sandbox**, on Darwin 27.0.0 arm64 with `codex-cli 0.160.0`. The owner reports
the real executable's SHA-256 prefix `112fae7a5a1223e6`, matching the team's
full hash below; the owner did not supply a full hash or resolved path.
This is attributed owner execution evidence, not a team rerun or fixture.

The Python probe tested bind on `127.0.0.1:0`, followed by connect and receive;
TCP connect to `1.1.1.1:443`; TCP connect to `8.8.8.8:53`; a UDP DNS query to
`8.8.8.8:53` with a reply read; and resolving `example.com`.

| Experiment | Recorded command/configuration | Observed result |
| --- | --- | --- |
| A: closed network | `codex sandbox -P`; `enabled=false`, `allow_local_binding=true` | Every probe refused with EPERM, including loopback bind |
| B: enabled with local destination rules | `codex sandbox -P`; `enabled=true`, `allow_local_binding=true`; domains `localhost`, `127.0.0.1`, `::1` allowed | Loopback round trip succeeded; off-machine TCP 443 and TCP 53 connected; UDP 53 returned a 61-byte reply; DNS resolved |
| C: proxy/SOCKS variant | B plus `proxy_url=http://127.0.0.1:3128`, `enable_socks5=false`, `mode=full` | Same full egress as B; no proxy variables were set inside the sandbox |
| D: actual native session | `codex exec -m gpt-6-luna`; `-c` overrides `default_permissions` and `permissions.<p>.network` with `enabled=true`, `allow_local_binding=true`, domains `localhost` and `127.0.0.1` allowed; `approval_policy=never` | The model ran the probe; same result as B, including off-machine port-53 egress |

These are the complete settings and command details supplied by the note, not
reconstructed runnable commands. It does not supply full TOML, profile names,
disposable-home setup, credential details, raw command output, proxy-provider
traffic or thread IDs. Experiment D establishes model-driven native execution
via `codex exec`; it is not an app-server start/resume/readback transcript or a
dummy-credential scripted-provider experiment. A successful UDP reply is
positive off-machine receipt evidence, not an inference from a timeout.
The owner did not provide separate unsandboxed control logs or independent
host-to-sandbox inbound nonce observations. Those omissions cannot yield a
successful native loopback proof; observed egress already rejects B, C and D.

The owner's conclusion and instruction are:

> Codex 0.160.0 offers no loopback-only configuration. The network is either
> fully closed (loopback included) or open to every destination, whatever the
> domain allowlist. Keep harness.Support(Codex, Session, Loopback) refused,
> update the reason to cite these results, and finish the design doc with them.
> No further team experiment is needed.

This settles the task's **refusal delivery**. The diagnostic cites the measured
macOS results without converting that conclusion into proof about untested
configurations or platforms. Earlier draft-1 statements that native observations
were entirely unavailable and further team research was required are superseded
by this note and the current brief. After-landing native configuration experiments
remain owner checks; no new owner decision or pre-landing experiment is requested.

## Team prerequisite attempt, 2026-10-04

The team's nested-sandbox prerequisite was refused. No inference, owner account,
credential or off-machine connection was used by the team. This refusal is
separate from the owner's unsandboxed A–D results above.

| Property | Observed value |
| --- | --- |
| Platform | macOS, Darwin 27.0.0, arm64 |
| Kernel | xnu-13432.1.9~1/RELEASE_ARM64_T6000 |
| CLI entry | `/Users/paul/.local/bin/codex` |
| Resolved executable | `/Users/paul/.codex/packages/standalone/releases/0.160.0-aarch64-apple-darwin/bin/codex` |
| Version | `codex-cli 0.160.0` |
| Executable SHA-256 | `112fae7a5a1223e673c8a1791d32338f37df8b527ff1159bb8adac6c4dbf1b4b` |
| Go | `go1.27.1 darwin/arm64` |
| CLI help | `codex sandbox --help` exposes permission profiles, TOML overrides, `-C` and sandbox-state options |
| Profile probe | Exit 71, `sandbox-exec: sandbox_apply: Operation not permitted`; no command output |
| Outer local listener | `bind(127.0.0.1:0)` refused: `PermissionError: [Errno 1] Operation not permitted` |

The standalone sandbox attempt used a disposable home and work directory under
this repository, with `CODEX_HOME` set only in the child environment. No login
files or owner configuration were copied. The directories were removed after
the subprocess settled. Complete experiment configuration:

```toml
default_permissions = "loopback_research"
[permissions.loopback_research.filesystem]
":root" = "read"
[permissions.loopback_research.filesystem.":workspace_roots"]
"." = "write"
[permissions.loopback_research.network]
enabled = false
allow_local_binding = true
```

Command (paths replaced with disposable-directory placeholders):

```sh
CODEX_HOME=<disposable-home> codex sandbox -P loopback_research \
  -C <disposable-work> /usr/bin/true
```

This was a nested-sandbox prerequisite check only. Its failure is not evidence
that `allow_local_binding` was honored, ignored or sufficient. There was no
successful sandboxed bind, reach, inbound receipt or denial observation.
An earlier `codex sandbox macos --help` attempt likewise reached Seatbelt and
failed with `sandbox_apply: Operation not permitted`; it supplied no subcommand
help or policy evidence.

Identity was collected with `codex --version`, `uname -a`, and Python's
`os.path.realpath` and `hashlib.file_digest(..., "sha256")` on the executable.
These operations do not read login material.

## Source evidence versus execution evidence

The task's source finding identifies
[`rust-v0.160.0/codex-rs/sandboxing/src/seatbelt.rs`](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/sandboxing/src/seatbelt.rs),
specifically `dynamic_network_policy_for_network`: local binding together with
proxy ports grants unrestricted outbound port 53. This finding is attributed
to the task's tagged-source research; this network-disabled environment could
not independently retrieve or hash that source. It explains why a proxy
destination allowlist alone cannot establish the requested policy. It is not
runtime evidence by itself and does not establish Linux behavior. The separate
owner experiments above do establish off-machine egress for their tested macOS
enabled-network configurations.

Configuration references for future proofs are the installed CLI's
[permissions documentation](https://learn.chatgpt.com/docs/permissions),
[configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
and [security documentation](https://learn.chatgpt.com/docs/agent-approvals-security).
Those pages were not fetched here. Proposed fields and fixture responses are
not runtime evidence.

| Evidence dimension | macOS observation | Linux / Windows observation |
| --- | --- | --- |
| Closed network and local binding, standalone sandbox | Owner A: loopback and every network probe EPERM; team prerequisite separately refused | Not executed |
| Enabled network and local destination rules | Owner B: local round trip and full off-machine egress | Not executed |
| Proxy/SOCKS variant | Owner C: full egress with proxy URL, SOCKS disabled and full mode; no proxy variables | Not executed |
| Other proxy/UDP alternatives | Not reported; no further team experiment required for refusal | Not executed |
| Actual native command execution | Owner D: model executed probe via `codex exec`, with same egress as B | Not executed |
| Actual app-server execution/start/resume/thread-read | Not supplied; do not equate `exec` with app-server readback | Not collected |
| Dummy-credential scripted-provider communication | Not supplied; owner D used a model; team's local provider could not bind | Not collected |
| Local bind/reach/receipt | Owner B–D: loopback round trip; no separate host-to-sandbox inbound nonce evidence | Not proved |
| Direct off-machine TCP/UDP, including port 53 | Owner B–D: TCP 443/53 connected, UDP 53 reply; A refused probes | Not tested |
| Native loopback capability offered | No | No |

There is no thread readback or raw tool transcript supplied. Owner-reported real
native execution is recorded above. Missing observations are unknown, never
enforcement success. Synthetic lifecycle regressions prove refusal before
credentialed launch, not Codex network enforcement. Installed-CLI research is
not added to automated tests.

## Requirements before any future enablement

These requirements gate future native support, not this refusal delivery. The
owner's current instruction requires no further team experiment. Keep the
after-landing experiment checklist separate from team checks.

Reuse the session profile/probe machinery, disposable homes, dummy credentials
and a scripted local provider. Preserve native tools, filesystem restrictions
and inference. Compare supported variants in both `codex sandbox` and actual
app-server command execution, including start, resume and thread readback;
record every effective configuration and resolved binary hash. Refuse displaced
profiles or widened readback before a user prompt.

Prove bind, reach and host-to-sandbox inbound receipt with independent nonces.
Include 127.0.0.0/8, ::1, mapped IPv4 and available local interface addresses;
other private/LAN machines are off-machine. Provider communication and local
access cannot compensate for an escape.

Require successful unsandboxed witness controls and actual enforcement-denial
observations for direct, non-proxied TCP and UDP on port 53 and ordinary ports.
A timeout, UDP connect success, silence or absent DNS answer proves neither
denial nor containment. An unreachable control refuses proof. Reject the
unsandboxed negative control and any known DNS-permitting configuration.
Do not patch Codex, install privileged filters, accept the DNS exception or
widen production permissions to satisfy a canary.

Only complete, settled proof can enable a platform. Bind future cache evidence
to binary content, OS, effective configuration, network policy and proof
revision; bind newly enabled loopback references to policy/revision and reject
incompatible resume. Existing metadata-based session cache identity and
reference formats are unchanged in this refusal delivery. They must not be
used as sufficient evidence for future loopback. Cancellation and incomplete
observations must not publish success. Retain private-login, process-identity,
recovery and browser safeguards.

LAH-24 can use these address semantics and port-53 proof requirements, but must
not assume native loopback is established. Strict native validation on macOS,
both Linux bubblewrap versions and Windows remains separate from sandboxed
test skips and cross-compilation.

## Verification and plan accounting

Local macOS `go vet ./...` and `go test -race ./...` passed with `GOPROXY=off`,
`GOTOOLCHAIN=local` and a repository-local temporary directory. Environmental
refusals remain subject to the existing testenv probes; this was not an
unsandboxed `AGENT_HARNESS_TEST_NO_SKIP=1` run. All Windows packages compiled
using `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...`;
Linux packages also compiled with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`
and the same test command. The `true` executor does not run either platform's
test binaries. Linux runtime checks,
Windows runtime checks and cross-platform strict race CI were not run locally.
Cross-target `go vet ./...` passed with the same Linux and Windows settings.
The existing CI matrix retains strict vet/race checks on macOS, Windows,
Ubuntu 24.04/bubblewrap 0.9 and Linux/bubblewrap 0.8.0. This remains a team check;
it is not assigned to the owner because this executor is macOS-only.
The revised refusal's local full race suite and uncached targeted regressions
passed. The daemon-hosted project `run_check` also passed (exit 0, no timeout),
including vet and the race suite. An initial snapshot copy collided with a
runtime socket and an unsettled test directory; after local test settlement
and clearing the socket artifact, the retry completed successfully. These are
check-sandbox results, not native loopback proof.

| Plan item | Result |
| --- | --- |
| 1. Research matrix | Done for refusal: owner A–D results, command/settings detail, binary identity and unavailable validation attributed separately |
| 2. Configuration alternatives | Owner A–C reject tested variants; owner directs no further team experiments; untested alternatives not claimed safe or impossible |
| 3. App-server comparison | Owner D establishes native exec behavior; app-server/readback not supplied and recorded honestly; additional team experiments waived by owner |
| 4. Network witnesses | No new canary needed for an Unsupported feature; owner probe records local round trip and off-machine TCP/UDP success; command semantics preserved |
| 5. Integration or refusal | Done: specific refusal cites installed-runtime gap; Start/Resume/VerifySandbox refuse before credentialed work |
| 6. Capability/platform gating | Done: Unsupported everywhere; macOS observations distinguished from other platforms lacking proof |
| 7. Cache/reference binding | No successful loopback policy to bind; existing formats preserved; future enablement must distinguish network policy and proof revision |
| 8. Regressions | Done for refusal: diagnostics and pre-launch lifecycle covered; no new enforcement/fault-injection proof claimed |
| 9. Documentation | README, session design, this evidence record and unreleased notes updated; no port API or breaking availability change |

The current brief permits the explicit-refusal outcome, and the owner explicitly
ends further team experimentation. No platform is offered without native proof.
No capability is enabled from configuration, source analysis, missing output or
synthetic tests alone. Missing app-server evidence and other platform execution
remain limits of the evidence, not an outstanding team research prerequisite.
