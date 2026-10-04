# Standalone command sandbox

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

macOS uses the existing Seatbelt localhost-only policy. When Loopback is set,
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
This is test infrastructure only: no sandbox profile, permissions or
harness.Support claims change. After landing the owner confirms the runner
unsandboxed on macOS and Linux with bubblewrap, and confirms an unsandboxed
NO_SKIP race run retains full coverage. Platform CI remains a separate check;
an outer prerequisite skip is not evidence that the real child fixtures
executed. The owner has confirmed there is no team-accessible Linux executor;
use the reported draft results below, and treat further Linux runs as owner
steps without reopening that executor question.

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
