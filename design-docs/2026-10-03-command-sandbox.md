## Standalone command sandbox

The sandbox package exposes Open without a model session. Its
options are WorkDir, RuntimeHome, Write, Read, Env, Loopback, Timeout and
Background. Run takes CommandRequest{Command, Dir, Timeout} and returns a
CommandResult{ExitCode, Stdout, Stderr, TimedOut, Truncated}. Start takes the
same request with no Timeout and returns StartedCommand with Stop, Done and
Result. Start reports process launch rather than application readiness.

## One implementation and proof

Both standalone execution and Workbench.Commands use sandbox.Runner,
configured by normalized sandbox.Options and sandbox.Proof, the proved layout,
environment, token, binary identity and output budget. Its Execute method
returns a typed result; session maps it back to the hosted tool's byte-compatible
JSON and existing error contract. There are no model prompts, credentials or
API calls in opening or running standalone commands.

Open normalizes options, proves the sandbox and uses OpenWorkspace before preparing its runner. No command state exists until proof succeeds. Refusals are the
sandbox.RefusalError and sandbox.ProofError; the workbench translates them
to its existing UnsupportedError and CapabilityError. A failed or timed
out proof is never recorded. macOS reuses evidence for matching binary identity
and launch configuration. Linux re-runs its full trial and canary on every
Open, launch and resume: recorded keys do not capture possible sysctl or mount
changes and never bypass verification. The Env denylist
landed in v0.20.0 is unchanged, including refusal of scratch, loader,
shell-startup, harness-owned and credential-like variables. Env remains in
proof keys and outside session Ref digests. Names in Env grant no filesystem
access. harness.Support is unchanged: standalone Run's Linux loopback option
does not claim session-wide loopback support.

## Network differences

macOS uses Seatbelt localhost filters, which permit binds and inbound on every
local interface. A server bound to 0.0.0.0 or a LAN address may be reachable
from other machines; outbound off-machine traffic stays refused.
LoopbackLocalOnly requires Loopback and refuses on macOS before probing with
loopback_local_only_unenforceable. Linux standalone Run proves it in a private
namespace, adding host-interface TCP/UDP binds that must get EADDRNOTAVAIL.
Existing plain Linux Loopback behavior and keys are unchanged. When Loopback is set,
proof requires outbound access to the probe's listener, the command's own
listener and an outside connection into a listener started by the command.
The outside client must read the disposable listener's random nonce, so a
different host listener cannot satisfy the proof. That observation contributes
the required inbound line. The probe cancels and waits for that client to
publish its result before judging the evidence. Off-machine
access must still fail. v0.22.0 introduced seatbelt-workbench-v4, so earlier evidence
cannot stand in for the inbound check; v0.23.0 retains that key. Started servers are reachable from the
host; without Loopback, both serving and connecting are refused.

Linux always creates a private network namespace with an active lo. A command
always has its own private localhost and never the host's. Loopback on Run
requires an additional own-loopback bind/connect canary. The existing host
listener and off-machine checks remain. The own-loopback canary requires
OpenBSD nc (for example the netcat-openbsd package); traditional and BusyBox
nc use incompatible listener syntax and are not supported. v0.22.0 introduced bwrap-workbench-v2 with Loopback in the key;
v0.23.0 retains it. No seccomp layer is added: the owner accepted
private lo even without Loopback, since it grants nothing outside the sandbox.

Start with Loopback on Linux refuses before launch with sandbox.RefusalError,
Operation "start", Code RefusedNotOffered and Unsupported availability. Its
reason explains host unreachability and recommends a server/client in one Run.
No forwarder, shared network, pasta, slirp or session-long namespace is added.
Start without Loopback can run background work until Stop or Close.
Workbench.Commands.Loopback remains refused on Linux; shared session networking
is still a separate design. Windows and other OSes retain the workbench's
pre-launch command-sandbox refusal.

## Ownership, settlement and recovery

RuntimeHome is mandatory, private and outside the workspace. Sessions keep
RuntimeHome/sessions/<id>/ without migration. Standalone sandboxes use
RuntimeHome/commands/sandbox-<random suffix>/, with an exclusive lifetime flock. A short
parent-directory lock serializes sweeping and creation, so a newly created
entry cannot be swept before its lifetime lock is acquired. Independent live
entries remain locked and untouched. The parent lock wait honors context
cancellation.

Preparation retains the atomic workbench-token.json write and private
workbench/home and workbench/tmp. A crash after a durable token leaves an
unlocked entry for the next Open to sweep. An interrupted write leaves only
workbench-token.tmp; it is removed with the stale entry. Token inspection or
sweep failure keeps the entry while a new Open proceeds independently.
Malformed tokens also preserve uncertain ownership. Proof failure occurs
before this state preparation.

On launcher SIGKILL, Linux bwrap children die with --die-with-parent. macOS
supervisors and children retain the marker for the next stale sweep. A free
lock alone never authorizes signalling: SweepToken establishes live identity.

Public admission validates the directory without holding its mutex, then
re-checks closing under the admission lock, returns any directory refusal,
then checks the 64-command limit. A slow
filesystem walk cannot delay closing or settlement of other commands.
Public admission tracks active calls under a mutex and limits them to 64;
the shared runners also track live process sets, including macOS supervisors
retaining background jobs. Close sets closing before cancelling calls, waits
for settlement, closes runner processes, sweeps and removes state, then releases
the lock. A Start racing Close is stopped before it returns. Repeated Close
returns the same error. Failed sweep/removal returns CommandCleanupUnknown and
retains recovery state; the lock is released. Later calls are refused as
CommandSandboxClosed.

Run timeouts stop and settle the tree and return TimedOut with exit -1;
caller cancellation returns ctx.Err after settlement. Close cancelling admitted
work with a still-live caller context returns CommandSandboxClosed; caller
cancellation retains its context error even during Close. The hosted adapter
maps CommandSandboxClosed back to context.Canceled on both OSes, preserving
the existing hosted cancellation contract. Missing private status
returns CommandOutcomeUnknown rather than inferred success. Linux rechecks
the proved binary fingerprint before each execution and refuses replacement
as CommandStartFailed. StartedCommand.Done closes on settlement; Result waits
and returns bounded output and observed exit. Stop cancels and waits, becoming
a no-op once settled. Cancelling a live handle yields a context error, whose
result must not be interpreted as an exit observation. Output capture drains
after its per-stream 64 KiB cap; hosted workbench calls keep their budget-derived
limit. macOS Run retains its established background-job behavior; Start waits
for the group to settle too.

## Verification and release

Tests cover the tool adapter, public lifecycle and admission under the race
detector, invalid options before state creation, real own-localhost suites,
Linux host isolation, macOS inbound reachability, timeout, cancellation,
bounded output, recovery locks and Close reaping. Platform refusal guards
remain enabled locally and forbidden in CI. CI runs macOS, Windows and Linux
with distribution bubblewrap and 0.8.0. The standalone API shipped in v0.22.0. v0.23.0 moves it to sandbox.Open,
retaining session.OpenCommandSandbox as a Deprecated wrapper for one release;
consumer adoption is separate and requires no breaking migration.

See [the final sandbox package boundary](2026-10-03-sandbox-package.md) for
ownership, the split verification cache, error translation and compatibility.

### Checking the library inside the command boundary

The test suite probes refused prerequisites through internal/testenv: nested
Seatbelt, the Linux full-flags bubblewrap trial, Unix sockets under TMPDIR,
loopback listen/connect, writes outside allowed scratch, reserved atomic
workbench temporaries, and access to system paths needed by mount fixtures. The socket probe
checks session/privatefs.go's 90-byte channel-directory limit before binding;
bare EINVAL and other unexpected errors remain failures. Shared probes cache
one verdict per test binary, including concurrent callers. A partially completed
socket probe closes its listener and removes its directory; a killed probe can
leave a random owner-only ahp-* directory; an interrupted atomic-write probe
can similarly leave ah-write-* scratch. Successful and refused probes clean
up their temporary directories.

The CI re-exec fixture opens the real command sandbox with padded private
scratch and checks that nested-sandbox and Unix socket-path tests (plus loopback-bind on macOS;
Linux allows the private namespace's own localhost) exit zero with named SKIP reasons. It repeats them with NO_SKIP=1 in the command
string and requires FAIL and “forbids skipping”. Env neither inherits nor accepts
AGENT_HARNESS_* variables. Timeouts, truncation and interrupted fixture runs
fail the assertion; Run settles its tree and Close removes the runtime state.

The opt-in `go run ./internal/cmd/sandboxcheck` runner runs vet and race tests
from the module root with the cached toolchain/modules, no downloads, and a
private GOCACHE. Cancellation settles Run before Close; SIGKILL may leave
scratch, with interrupted command state swept on the next Open of that home.
This paragraph records historical LAH-23 infrastructure and owner observations.
Owner note 20 now assigns platform evidence to the owner's outside-sandbox
strict suites and sandboxcheck. Unposted results remain unknown; an outer
prerequisite skip proves no enforcement. Existing CI jobs are unchanged.
The separately agreed after-landing macOS undertaking does not replace it.

Linux sandbox-check follow-up: the unsandboxed privilege escape control probes
/proc/self/status. Zero effective, permitted, inheritable and ambient capabilities
with NoNewPrivs=1 prove inherited confinement; only the privilege subtest skips
with a named one-line refusal. Filesystem, network and overlay assertions still
run. Missing or malformed status fails; permission-denied reads name their own
diagnostic. Real sandbox canaries still require privilege-ok.

Reclamation lease tests use a ready, live survivor in a dedicated group greater
than 1 instead of the inherited test group (which can be 1 under bubblewrap).
Early worker errors fail immediately; cancellation settles the worker, releases
the lease and preserves the launch marker. No production ownership rules change.

Nested refusal fixtures require the platform-specific capability prefix and a
nonempty diagnostic suffix. Linux uses bubblewrap 0.8.0+ and unprivileged
namespaces; abbreviated reports are not complete diagnostics. Joined refusal
errors are flattened to one line. NO_SKIP still fails refused prerequisites;
child normal/strict fixtures select their own mode independently of the parent.
Production sandbox permissions and harness.Support claims remain unchanged.

### Interruption and regression coverage

Every Linux prerequisite callback checks ctx.Err() before adding a permission
refusal. RequireBwrap checks the context before admitting a trial and again
after settlement, so cancellation and deadlines fail even if a callback returns
a recognized CLI refusal or a joined permission error. The equivalent nested
Seatbelt probe also gives interruption precedence over refusal classification.
An interrupted trial proves neither support nor a refused capability.

Synthetic CLI fixtures cancel before launch, during version discovery and
during the full-flags trial. Normal-mode tests require a loud cancellation
failure, not a skip. Synthetic status files execute the privilege command
extracted from the generated production canary: confined status reports
privilege-ok, while NoNewPrivs=0, each nonzero relevant capability, missing,
duplicate, malformed or unreadable fields report privilege. Production
enforcement and mandatory privilege-ok observations remain unchanged.

The lease survivor responds to a ping after reclamation cancellation settles.
Unlike kill(0) or group occupancy, a response proves the child is executing and
excludes an unreaped zombie. Startup/readiness and responses are bounded by the
existing fixture context; cleanup awaits reclamation before killing/reaping it.
The test retains exclusion, both cancellation error identities, marker
retention and lease-release assertions.

CI pins the distribution runner to Ubuntu 24.04 and records OS/kernel,
architecture, Go and the installed bubblewrap version, requiring 0.9.x.
Strict vet/race coverage remains on macOS, Linux and Windows; bind-mount
witnesses and the authenticated upstream 0.8.0 job remain intact. Unix CI runs
the real re-exec fixture with parent NO_SKIP=0 and =1, requiring PASS plus
verified child results for every normal/strict mode. An outer skip fails this
CI step. Unix CI repeats the lease test twenty times under the race detector;
the Ubuntu job also runs the full sandboxcheck runner.

### Validation evidence

Owner task note 6 supplies the paired aggregate outcomes below. Runs were
outside a sandbox on Ubuntu 24.04 with bubblewrap 0.9 and on macOS.
Sandboxcheck means `go run ./internal/cmd/sandboxcheck`; the strict suite
means the reported `AGENT_HARNESS_TEST_NO_SKIP=1 go test -race ./...` run.

| Snapshot | Platform | Owner-reported outcome |
| --- | --- | --- |
| Baseline 106c711 (LAH-18) | Linux | Sandboxcheck exited 1: the unsandboxed privilege control failed at workbench_command_linux_test.go:653 with “escape privilege not reported”; the lease test failed at bridge_test.go:774. The strict race suite failed the nested refusal fixture at check_fixture_unix_test.go:86. |
| Draft 1 ad4b36b | Linux and macOS | Sandboxcheck and the strict race suite exited 0. |
| Draft 2 b54ec5d | Linux | Sandboxcheck and the strict race suite exited 0. Aggregate draft-2 Linux success is established by this report. |
| Draft 2 b54ec5d | macOS | Sandboxcheck exited 0. The strict suite failed TestCommandSandboxRunsSelectedDeveloperTools when initial xcrun/xcodebuild startup exceeded its 10-second command limit. |
| Main correction 71e7d39 | macOS | The owner reports correcting the developer-tool timeout and obtaining a passing test. This is a separate test result, not a reported full strict-suite rerun. |

Correction 71e7d39 is now merged into this task branch.
`TestCommandSandboxRunsSelectedDeveloperTools` gives its fresh sandbox one
minute for first-launch developer-tool startup, while retaining the command,
exit-status and built-output assertions. Preserve that merged correction on
landing; this task does not recreate it or change lease deadlines.

Test correction: `TestCommandSandboxGitMetadataOutsideScratchIsProtected`
warms `/usr/bin/git`'s developer-tool shim in the same sandbox outside the
measured runs, with a one-minute ceiling. Its protection commands retain an
explicit ten-second budget. Warm-up errors, timeouts and nonzero exits fail
without retries or skips; a measured timeout also fails rather than counting
as a refused write. The private-repository control and host-side absence
check remain unchanged. This correction adds no capability claim or
owner-reported validation result.

Failure classifications remain precise. The privilege control is unavailable
only after the probe observes zero CapEff/CapPrm/CapInh/CapAmb and NoNewPrivs=1;
that predicate proves inherited confinement. The baseline lease fixture used
the test process's inherited group, which could supply Group<=1, an identity
production correctly refuses. The Linux refusal matcher was a source-confirmed
test defect: it required the word sandbox rather than the complete bubblewrap
capability prefix and diagnostic suffix. These explanations and the reported
baseline failures are distinct from raw measurements.

The record does not include actual baseline privilege fields, PID/PGID,
reclamation worker error, complete child output, or separate focused and
both-parent Linux transcripts. Aggregate strict-suite success is evidence for
the suite's assertions, not a captured transcript of those individual checks.
Sandboxcheck success alone does not establish that an outer re-exec prerequisite
passed rather than skipped. Owner Go/kernel/architecture details and
timeout/truncation metadata for these reports were not supplied. The task API
abbreviates note 6; its complete stored text was recovered during research and
the evidence inventory was supplied with this reconciliation request.

Prior team checks on the repaired drafts ran on macOS arm64 with Go 1.27.1
inside managed command boundaries: local vet/race and daemon run_check exited
zero, including twenty lease race repetitions and the synthetic regressions.
Windows/Linux amd64 tests cross-compiled CGO-free, cross-target vet and a
CGO-disabled library build passed; compilation is not runtime evidence.
Local real re-exec attempts encountered a refused outer loopback prerequisite:
normal mode skipped (zero), strict mode failed (one). Local sandboxcheck
refused before launch because managed TMPDIR was beneath the checkout.
These infrastructure limits are not Linux namespace-refusal findings.

Windows and upstream bubblewrap 0.8.0 runtime CI success, separate both-parent
execution transcripts, and CI results for the landing revision remain
unrecorded. CI configuration is not execution evidence. The owner's paired
baseline/draft outcomes replace the obsolete requirement for a nonexistent
team Linux executor; any further Linux diagnostics follow the settled owner
direction. Additional diagnostics, if requested, use disposable copies and
retain the assertions. The expressly designated after-landing Ubuntu check
remains separate, and this repair retains priority before the held main push
and subsequent library landings.

## Readable PATH and execution correction (LAH-25)

Source baseline: command profile `seatbelt-workbench-v7` granted unrestricted
process-exec; merged inherited/caller PATH was unfiltered. The baseline commit
was not inspected because this task prohibits touching `.git`; the task system
records the repository snapshot. LAH-14 and LAH-23 are landed prerequisites.

Supplied owner baseline evidence (task note 4), macOS 27 outside any sandbox,
reproduced 2026-10-04 with library v0.23.3: within sandbox.Open, PATH placed
`~/.nvm/versions/node/v22.22.3/bin` before `/opt/homebrew/bin`.
Both `npm --prefix internal/dashboard/ui run check` and `npx tsc --noEmit`
exited 139 (`Segmentation fault: 11`). `command -v node` resolved Homebrew node
because sh skipped the unreadable directory, whereas `#!/usr/bin/env node`
executed the nvm node. Removing home-directory PATH entries allowed the check
and all 734 dashboard tests to pass inside the sandbox. This is supplied real
baseline evidence, distinct from the team's refused synthetic attempt below;
it does not establish the corrected draft's permission refusal or Linux behavior.
The supplied note's draft-1 strict-check excerpt is truncated, so no further
draft-1 enforcement result is inferred from it.

Team synthetic baseline attempt, 2026-10-04: Darwin 27.0.0 arm64, with
`/usr/bin/sandbox-exec` SHA-256
`58839ef01b4eef8aac0d2aa8f9d1c074ae45aafe3533965b030672450064acc8`.
A disposable copy of `/bin/sh` at `.task-tmp/hidden-node` was launched directly
under `(version 1)(deny default)(allow process-exec)(allow process-fork)` with
reads only for `/System`, `/usr`, `/bin` and `/dev`. Its arguments were
`-c 'echo started; cat .task-tmp/runtime'`; inherited PATH was not relevant to
this direct launch and was not recorded. Output was exactly
`sandbox-exec: sandbox_apply: Operation not permitted` before child execution.
This is an enclosing-environment prerequisite refusal, **not experimental
reproduction of the defect**. No runtime-loading failure or corrected OS errno
has been observed in that attempt. The fixture was removed. The real regression
reconstructs unrestricted process-exec, retaining restricted reads, and requires
an independent startup marker even when the subsequent runtime read fails.
The current widened regression grants unrestricted process-exec for installed
/bin/echo under a separate narrow read policy. Individual /bin/sh dispatcher
interpreter grants permit the supervisor to start while /bin/echo remains
unreadable. Unauthorized native startup must produce outside-native-started and
be rejected as not enforced. The script PATH-selection controls are separate;
this negative control neither copies a native shell nor restores nvm-first PATH.

The effective read policy remains System/toolchain, explicit Read, workspace
and private Home/Tmp, with platform exclusions. Metadata-only parents and
file-only grants cannot admit PATH directories. After environment overrides
replace inherited values, each command rechecks PATH. Existing absolute
directories whose canonical targets are covered are retained canonically in
order, including duplicates. Empty/relative, missing, non-directory,
unresolvable (including dangling/looping links) and outside entries are dropped.
No access is granted to make a tool usable. Missing/all-dropped PATH uses a
fresh empty directory under already-readable private Tmp, rather than shell
defaults or current-directory lookup. Preparation failure or an unrepresentable
scratch PATH (including a separator in its name) refuses launch.
The runner captures the source environment once; per-invocation filtering and
diagnostic buffers are independent and never mutate the parent or source slices.

One `[harness PATH: ...]` note begins result stderr when entries are dropped.
It quotes paths, uses fixed reasons and counts omitted entries. The note consumes
the existing per-stream budget, including hosted minimum budgets; subsequent
output uses the remaining space and the existing truncation flag/marker.
Run, Start/Result (including repeated reads) and Runner.Execute retain one
bounded escaped stderr report on nonzero exits, timeouts and settled errors.
Pre-launch refusals have no command report. Shared Start returns nil before
launch and a settled non-nil handle on post-launch failures, even when caller
cancellation or Close races launch. Result is stable and does not imply rollback.
Hosted settlement and deprecated wrappers now preserve the shared diagnostics
on failures as well as success. The API workbench design describes bounded JSON
failure payloads, compatibility handles and unchanged unknown-effect accounting.
Only the retained session-specific hunks were adapted after LAH-25 and LAH-32;
the shared policy and file-tool containment remain intact.

Scratch admission captures an os.Root descriptor and identity for private Tmp
before commands start. Empty-PATH fallback creation uses Root.Mkdir with unique
names, never pathname-based MkdirTemp. Path identity, canonical location,
privacy and owner are checked before/after creation; changed paths refuse
preparation and clean through the anchor. Symlink replacement cannot direct a
host write into its target. Concurrent replacement regressions assert no outside
directory is created. Descriptor anchoring does not claim elimination of all
filesystem races or stronger hard-link/injected-mount containment than the OS
boundary provides. All-dropped PATH never restores inherited/cwd lookup.

PATH fixtures use #!/bin/sh scripts; no copied Apple shell or signing tool.
Darwin independently probes installed /bin/echo at its original signed path,
using a disposable read policy that excludes /bin while retaining /bin/sh as a
file-only interpreter grant. This exercises the same read/execute selector
emitter without altering command grants. Native positive startup outside the
boundary is required, followed by an OS permission refusal before native startup
inside. Direct and replaced-directory cases are logged. Widening only process-exec
must allow the native marker and be rejected as not-enforced. Script-read denial
alone never certifies native execution confinement. Linux uses a copied ELF echo
fixture outside the unchanged bind set, requiring actual status 126/127 plus
inaccessible-path or permission diagnostics and no startup marker.

ProofError.Step and harness.Facts.ProofStep expose fixed sanitized values:
fixture_preparation, outside_control, sandbox_launch and execution_judgment.
Open errors name the failing step, without paths, output or credentials. Missing
positive controls remain unavailable; native startup remains not-enforced;
interruptions remain probe_timed_out and retain the step. No failed/interrupted
proof records cache evidence or prepares command state. Compatibility propagation preserves the same fixed facts. Current identities are seatbelt-workbench-v11, bwrap-workbench-v4 and
readable-path-v2; draft-4 v9/v3/v1 keys are rejected, profile/mount pins remain
unchanged, and Linux still re-proves. Network rules, atomic recovery, uncertain
ownership, admission/Close semantics and harness.Support claims are unchanged.
LAH-21 reuses this corrected policy; native networking is LAH-19/LAH-24 and access
requests remain LAH-22. LAH-26 containment is separate; restoration is not a
prerequisite.

Evidence inventory by snapshot:

- Note 4: supplied macOS 27/v0.23.3 baseline above, exit 139 with nvm-first PATH;
  removing home PATH entries allowed the check and 734 tests to pass.
- Note 10: supplied draft 3 `1b655a6`, outside any sandbox, Ubuntu 24.04 with
  bubblewrap 0.9.0: sandboxcheck and strict no-skip race suite both exited zero.
  macOS 27 arm64 real Open failed; owner instrumentation found the copied
  /bin/sh outside control SIGKILLed before startup. This is a product fixture
  failure, distinct from the team's environmental nested-Seatbelt refusal.
- Draft 4 `0f81bfc`: signed-copy startup passed locally on macOS 27.0 build
  26A428 arm64, local/daemon vet/race passed, but strict real canary hit nested
  Seatbelt permission refusal exit 71 before construction. Corrected enforcement
  and negative control did not run. Those fixture/signing results are historical;
  the approved candidate now uses scripts and an independent native witness.
- Current split working tree: tests and real-canary attempts are recorded below.
  Earlier Linux successes cannot certify this candidate or bubblewrap 0.8.0.
  No CI configuration, generic aggregate success or skip supplies missing
  execution/refusal evidence. Unsandboxed suitable runners remain required
  under owner note 20; future reruns are not completed evidence.

Acceptance audit:

| Criterion | Result |
| --- | --- |
| 1: platform vet/race | Current checks below; Linux/Windows runtime matrix acceptance remains pending. |
| 2: docs | README, command/package design and pending notes reflect the core split. |
| 3: claims | Support unchanged; failed/unrun probes certify nothing. |
| 4: shared mechanism | Existing System/Read/Work/Home/Tmp policy reused; hosted/compatibility adapters preserve the shared policy. |
| 5: PATH | Canonical ordered inherited/override filtering, symlinks, safe anchored fallback and replacement regressions. |
| 6: diagnostics | One bounded stderr note retained in shared Run/Start results. |
| 7: macOS | Script controls plus independent native permission-refusal and widened-control checks; real execution results pending. |
| 8: Linux | Mounts unchanged; separate native/refusal markers; candidate 0.9/0.8.0 results pending. |
| 9: regressions | PATH, env-shebang, external Read, replacement and shared command entry points retained; hosted synthetic and readable-PATH tests cover settlement. |
| 10: evidence/cache | v11/v4/v2, old-key rejection/stability, no failed-proof caching; Linux fresh proofs. |
| 11: release/dependencies | Core docs updated; existing downstream policy dependencies retained. |
| 12: review | Loopback-free execution tests retained; shared Start diagnostics preserved; owner baseline attributed. |
| 13: draft-3 defect | Script correction and native witness implemented; owner draft-3 Linux/macOS observations kept snapshot-specific. |

The separate after-landing owner confirmation remains: unsandboxed macOS baseline,
readable selection and corrected permission refusal. It does not replace any
verification requirement now assigned to the owner by note 20.

Current candidate local validation (working tree based on `0f81bfc`, extracted
against `181960c`, 30 changed files): local full `go vet ./... && go test -race
./...` passed after preserving legacy no-step error wording for the deferred
compatibility boundary. New step-bearing execution errors use the expanded
read/execute refusal explanation. Session and API-design paths have no diff
against the extraction baseline; retained originals are identified above.
Replacement/concurrent replacement, sanitized step failures and execution judges
passed ten race-enabled repetitions. Windows/Linux amd64 test packages compiled,
target vet passed and the CGO-disabled build exited zero (with a denied Go
module-stat-cache write diagnostic); these are not target runtime results.

The strict fixture test `AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -count=1
-v ./sandbox -run '^TestWorkbenchExecutionFixtureRunsAtDisposablePaths$'`
passed on macOS 27.0 build 26A428 arm64. Scripts executed at disposable locations,
including outside/runtime and readable interpreter/env controls; the installed
native tool's outside control also ran. The strict real execution-canary command
with the same flags and `-run '^TestWorkbenchExecutionCanaryReal$'` exited one
at `environment refuses a nested OS sandbox: sandbox-exec: permission denied
exit status 71 (AGENT_HARNESS_TEST_NO_SKIP=1 forbids skipping)`.
Native/script enforcement, replaced-directory refusal, widened negative control
and successful public Open therefore did not run locally. No corrected Linux
0.9/0.8.0 runtime transcript is available; network/CI dispatch are prohibited and
the daemon executor exposes no alternate platform. Historical owner draft-3
Linux success remains tied to `1b655a6`.

Local `go run ./internal/cmd/sandboxcheck` exited one before launch:
`work_dir: the workspace and the runtime home must not contain one another`,
because managed TMPDIR is within the checkout. It supplies no enforcement
evidence. The experimental CI capture machinery was removed under owner note 20;
existing CI jobs are unchanged. The owner will post outside-sandbox strict-suite
and sandboxcheck results for the ready revision.

Lifecycle audit additionally guards the closed low-level runner before scratch
inspection. The regression checks that Close prevents Execute from preparing a
PATH or returning diagnostics, including when Close returns cleanup_unknown.
The local synthetic runner encountered that existing process-inspection
uncertainty; the test preserves its code and makes no reaping-success claim.
Focused race regressions passed after this guard; Linux/Windows compilation
and Linux target vet passed. Final daemon validation is recorded separately.

Final project run_check for this corrected split, including the closed-runner
guard, completed with exit zero, empty stderr and no timeout/truncation. All
packages passed the daemon vet/race check. This does not alter the pending strict
platform enforcement/public-Open evidence listed above.

### Draft-5 review correction (2026-10-04)

The native Darwin trial now grants /bin/sh, /bin/bash, /bin/dash and /bin/zsh
individually. /bin remains excluded, as does /bin/echo; the existing real trial
requires the supervisor's readable positive control before accepting native
direct/replaced-path permission refusals. This fixes the missing interpreter
grants identified by review, without widening command policy or network access.
Seatbelt v11 rejects v10 evidence. The Linux draft-4 legacy regression now
freezes the actual single bwrap-workbench-v3:readable-path-v1 revision element.
The current widened control is described above rather than the historical
copied-shell control.

Darwin checks context after successful probe cleanup and before cache publication
or reuse. Public Prove also checks context before returning evidence. Synthetic
tests use the actual discovery, cache and Open admission implementation, replacing
only the disposable trial: successful cleanup cancels the request, returns no
proof/handle, records no key and leaves the supplied RuntimeHome empty. Failed
fixture/outside/launch/judgment trials likewise exercise this admission boundary.
These tests are enforcement-independent and do not claim real OS proof success.

Focused race tests and strict disposable-fixture startup passed on this revision.
The strict real Darwin trial exited 1 at its prerequisite with:
`environment refuses a nested OS sandbox: sandbox-exec: permission denied exit status 71
(AGENT_HARNESS_TEST_NO_SKIP=1 forbids skipping)`.
This attempt never reached the corrected narrow profile, native refusals, widened
control or public Open. Those suitable-runner acceptance results remain missing,
as do corrected Linux 0.9/0.8.0 strict results and Windows runtime acceptance.
No network or remote runner is accessible here. Historical owner draft-3 Linux
success and macOS fixture failure remain attributed to 1b655a6; none certifies
this revision. Prior aggregate run_check results above are historical, not proof
of this correction. The after-landing macOS undertaking is separate.

Final candidate check: macOS 27.0 build 26A428 arm64, Go 1.27.1.
Go-source/go.mod/go.sum bundle SHA256 (sorted relative filenames and contents,
each separated by a NUL): `c3c754cfc365d7e2f3fa9d82a35c6e1eef5788f5709bc462af1002f160514913`.
Local go vet ./... and go test -race ./... passed; focused cleanup/admission,
legacy-key and disposable-fixture race regressions passed after the last code
change. Linux/Windows amd64 target vet and all test-package cross-compilation
passed, as did CGO_ENABLED=0 go build ./... (the tool printed a module stat-cache
write refusal but exited zero). Final run_check completed with exit 0, empty
stderr, timed_out=false and truncated=false; all packages passed. These aggregate
results do not supply strict macOS/Linux enforcement or Windows runtime evidence.


### Final scope under owner note 20
Owner note 20 supersedes the earlier CI capture/dispatch plan and verification
ownership. The capture helper, workflow phases, provenance artifacts and failing
capture-run requirement are removed; CI matches the primary checkout workflow.
The owner will run strict suites and sandboxcheck outside the sandbox on macOS
and the Linux VM and post results. No successful corrected enforcement evidence
is claimed yet; baseline note 4 and draft-3 note 10 remain historical evidence.

The post-start/pre-Notify regression now uses the actual platform runner,
including its status parser, bounded output capture, settlement and Start/Result.
Only child launch/setup are synthetic, using internal per-runner dependencies
that public constructors cannot configure. Both missing status and fast successful
status are covered. A channel confirms fast status was parsed before the fixture
returns its setup refusal. The test requires an unknown outcome, retained handle,
real stdout, exactly one bounded PATH note, truncation and stable repeated Result.
Priority refusal, read/execute and network policy remain unchanged.


Final narrowed-revision validation (owner note 20):
Go-source/go.mod/go.sum NUL-separated bundle SHA256: `708b7dbf2b80728e5b18bbdff6602d26dcc8092084db6606f1c690095dee874f`.
The actual-runner post-start regression passed ten race-enabled repetitions,
including explicit parsed-success synchronization. Local vet, Linux target vet,
CGO-disabled build and all Windows/Linux amd64 test-package cross-compilation
passed (Go printed a denied module-stat-cache write diagnostic but exited zero).
Final run_check passed all packages with exit 0, empty stderr and no timeout or
truncation. The workflow was compared byte-for-byte with the primary checkout;
the collection helper is absent. These local checks do not prove macOS execution
refusal or Linux 0.9/0.8.0 enforcement and do not establish Windows runtime results.
The owner supplies outside-sandbox platform results under note 20; earlier
snapshots and environmental refusals remain historical and cannot certify this
revision. No hosted/compatibility work was reintroduced from LAH-30.
