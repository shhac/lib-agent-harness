## The sandbox package

See [process inspection](2026-10-05-process-inspection.md) for the opt-in
own-command-tree requirement, capability/refusal matrix and evidence inventory.

## Containment contract, 2026-10-04

On Linux and macOS, `read_file`, `search_files` and `edit_file` are
temporarily not offered: “workbench file tools are off until their workspace
check is verified”. Standalone `Workspace.Read`, `Search` and `Edit` return
a typed `*sandbox.RefusalError` (`not_offered`, capability family,
`Unsupported`) before parsing arguments or performing content I/O.
There is no opt-in bypass. `list_files`, opt-in atomic `write_file`, skills,
caller tools and proved commands continue.

API workbench `Start`, `Open` and `Resume` continue with this reduced
surface. `Session.Capabilities().WorkbenchTools` lists each configured tool
with its availability and reason, returned as a defensive copy. A generated
system notice tells the model which content tools are absent; advertisements
and hosted admission use the same effective definitions. All six names remain
reserved. Forced calls are refused before handlers run and do not close
admission; an ordinary closing tool can still succeed.

`harness.Support` reports aggregate `WorkspaceRead` and `WorkspaceWrite`
as unsupported on Linux/macOS because their full tool sets are unavailable.
This does not deny the surviving listing or write tool, or change command
claims. Windows retains its existing file-tool behavior. Configuration digests
and reference formats do not change: resumption reconstructs availability,
preserves historical content results, and never replays old calls. The notice
is regenerated outside durable transcript content and does not accumulate.

Restoration requires a reviewed storage/admission invariant against concurrent
links, renames, unlinks and external mutation (LAH-27 design, LAH-28 implementation);
passing repetitions or independent stat samples cannot establish that invariant.
Consumer migration is tracked separately in LAH-33/34 under the approved split.
The [storage/admission design](2026-10-05-workspace-storage-admission.md) is the
review prerequisite for LAH-28: dedicated local filesystem roots only, ordinary
directories still refused, implementation deferred until consumer demand.
This design changes none of the behavior or historical evidence below.

The historical read/admission descriptions below describe the retained
implementation, not current Linux/macOS authorization.


The OS sandbox has a package boundary apart from session orchestration,
tool admission, telemetry and transport. `github.com/shhac/lib-agent-harness/sandbox`
is part of the existing module, not a separate module. Stage A extracted file
access; stage B completes the command extraction and ships in v0.23.0.

## Ownership

Sandbox owns confined workspace reads, listing, search, atomic writes and edits,
reserved names, link/mount checks, directory walks, platform durability and its
serialized I/O worker. `OpenWorkspace(Config)` returns `Workspace`.
`Read`, `List`, `Search`, `Write` and `Edit` accept workbench JSON and return
`Result{Content, IsError}` plus an error. `Definitions` supplies descriptions and
schemas; callers decide which operations to offer and authorize.

Config carries root, effective file mode, result budget, session ID, cancellation
grace and OnFailure. Zero budget means 64 KiB; zero grace means ten seconds.
Session normalizes mode and host budget. `ResolveName` and `Workspace.CheckDir`
validate command directories without exposing handles. `Workspace.RemoveReserved`
removes only one session's interrupted temporaries. `SyncDir`, `Nested`,
`Within` share durability and path rules with native CLI callers. Read-directory
normalization is shared internally.

Sandbox also owns Seatbelt/bubblewrap command profiles and arguments, tool
version and namespace checks, canaries, judges, proofs and command verification
cache, per-platform runners/supervisor, bounded output, environment normalization,
tokens and standalone recovery state. It imports no session, completion, catalog,
account or native package. The import-boundary test checks transitive dependencies.

Session retains `Workbench`, `Commands`, workbench tool selection and hosting,
`run_command` schema/JSON, transcript accounting, retries and recovery decisions.
Its `workbenchHost` holds a `sandbox.Workspace` and `sandbox.Runner` plus session
metadata. Unknown command outcomes retain the original hosted sentinel; closing
command admission maps back to context.Canceled. `workbench_cleanup.go`, Ref
digests, `workbenchDigest`, `session.Sandbox` and native CLI probes stay in session.

`process` stays public and top-level because it contains native CLI harness
processes and account/Grok execution as well as commands. `internal/wsfile`
stays internal and shared with skills. `internal/textbound` shares UTF-8 bounds.
`internal/sandboxprobe` contains the unchanged network witness/listener helpers
used by command and native CLI proofs; it owns no CLI transport or credentials.
`internal/sandboxhook` exposes only in-module observations and fault injection;
production session code does not use it.
`internal/sandboxbridge` shares read-directory rules and connects session to
sandbox-owned normalization, proofs and state-lock functions. Sandbox registers
the opaque Options/Proof adapters at initialization to avoid an import cycle.
These integration helpers are internal, not public compatibility commitments.

## Standalone and low-level APIs

`sandbox.Open(ctx, Options)` returns `*Sandbox` with bounded `Run`, long-running
`Start` and idempotent `Close`. Options are WorkDir, RuntimeHome, Write, Read,
Env, Loopback, Timeout and Background. Requests are `CommandRequest{Command,Dir,Timeout}`;
results are `CommandResult{ExitCode,Stdout,Stderr,TimedOut,Truncated}`.
`StartedCommand` provides Stop, Done and Result. Start reports launch, never
server readiness. Admission and the per-platform live process sets retain
the 64-command bound. Closing rechecks admission under its mutex, cancels
admitted calls, waits for settlement and reaps descendants before releasing state.

Internal normalization freezes standalone options. Its hosted entry point uses
the same mechanism while preserving the hosted Linux Loopback refusal and its original
precedence (timeout/home/read validation, platform restriction, environment).
WorkDir and RuntimeHome must be separate existing directories; RuntimeHome is
owner-private. Read aliases, Unix socket exposure rules and the Env denylist
retain their reasons and order.

`Prove(ctx, Options) (Proof,error)` uses disposable paths and returns an opaque
value with SystemDirs, Binary and Identity accessors instead of mutating Workbench.
SystemDirs returns a copy. Identity is informational on macOS: the cache key
already includes the executable hash and metadata, and the runner does not
recheck identity at launch. It is derived from the same binary read as that key,
without an additional failure path. Linux checks identity at runner preparation
and every execution. `NewRunner(Options,Proof,stateDir,outputBudget)` requires
options matching the original proof request or its frozen normalized value,
private state within RuntimeHome and either zero
(standalone per-stream output) or the hosted 4096–65536 byte result budget.
NewRunner uses the frozen options without repeating normalization, system
discovery or RuntimeHome validation after proof; it still validates its private
state directory and Linux executable identity. Prove normalizes public input;
hosted sessions use the internal proof entry point after their own normalization.
Zero or mismatched proofs are refused before preparing state. Hosted sessions
pass RuntimeHome/sessions/<id>; standalone uses RuntimeHome/commands/sandbox-*.
`Runner.Execute`, Close and Timeout expose the shared mechanism; callers own
directory validation, admission, the state lock and actual settlement.
Internal lock and private-directory helpers preserve native flock/LockFileEx
semantics and the session.lock filename. Ordinary standalone callers should use Open.

Linux always has a private network namespace and its own lo. Standalone Run's
Loopback requests a positive own-localhost proof; Start+Loopback refuses with
the same start/not_offered reason because the host cannot reach that namespace.
Hosted Linux Loopback remains unavailable. macOS retains inbound and outbound
loopback proof and denies outside access. Windows command APIs refuse before
launch; ordinary native sessions and workspace file access remain supported.

## Split verification cache

Session's process-wide `verified` cache now contains only native CLI sandbox
and restricted-runtime probes. Sandbox owns a distinct mutex-protected command
cache with identical holds/record behavior: record resets the map when its
existing length is greater than 64, then stores the new key. It can therefore
hold 65 entries before the following record resets it. Neither cache reads nor
writes the other's evidence. Restarting the process clears both; nothing is
persisted or migrated. Concurrent proofs can both run; recording is synchronized.

macOS alone reuses a successful exact binary/configuration key. Linux always
runs its installed-tool trial and full canary; its recorded key is evidence,
never authorization to skip a fresh namespace/mount/listener proof. Failed and
interrupted proofs record nothing. No verification bypass or new Support claim
is introduced.

The stage-B package extraction historically preserved the stage-A proof payloads
(Seatbelt v4 and bubblewrap v2) byte for byte. Subsequent command-policy changes
intentionally revised that evidence. Current command keys include
`seatbelt-workbench-v11`, `bwrap-workbench-v4` and `readable-path-v2`, with the
current Seatbelt template digest pinned. Regression tests reconstruct the old
payloads and require old-key rejection plus stable new keys; the bubblewrap
mount argument fixture remains unchanged. Standalone proofs still retain the
historical `sessions/id/workbench/home` and `sessions/id/workbench/tmp` shapes.

## Errors and compatibility

Sandbox errors are `RefusalError`, `ProofError`, `CommandError` and `StateError`,
plus ErrUnsupported, ErrClosed, ErrCommandFailed and ErrStateLocked. StateError
carries only StateUnusable or StateLocked. Error text and structural facts
exclude raw provider output and paths. Codes, actionable bwrap messages and
harness facts retain the prior vocabulary.

Every sandbox error reaching session goes through `fromSandbox`: refusal to
UnsupportedError; proof to CapabilityError(BeforeLaunch); command to TurnError;
state to StateError; ErrClosed to session.ErrClosed. Context errors pass through.
The workspace stuck latch preserves one translated failure for health and Release.

Published v0.22.0 has `session.OpenCommandSandbox`. For v0.23.0 it remains a thin
Deprecated wrapper over Open, with CommandSandboxOptions' public fields unchanged.
CommandRequest/CommandResult alias sandbox's types; CommandSandbox/StartedCommand
wrap it to translate errors, including settled handle errors and cleanup failures.
Repeated Close and Result retain the same translated error identity. Nothing
public is removed. crew-assistant and crew-code-review need no update; new
standalone use, including CA-62, should adopt sandbox.Open.

## Interrupted lifecycle and recovery

Open normalizes, proves, checks cancellation, opens workspace file access,
prepares command state under the base lock, then creates the runner.
Proof/cancellation failure creates no command state and holds no recovery lock.
Runner preparation failure closes the workspace and releases the lifetime lock;
its state remains for a subsequent sweep.

Standalone sweeping/creation serializes on commands/session.lock, waiting with
context cancellation. The process can die after MkdirTemp and before locking or
writing a token: next Open takes the entry lock and removes an entry without
workbench-token.json. A locked live entry is skipped. The legacy-format fixtures
exercise the same bytes written by v0.22.0, both through preparation and Open.

After token rename, workbench-token.json contains Token and Since. A sweep
establishes live identity through process.SweepToken, then removes the entry
only on success. Corrupt, unreadable or unconfirmed ownership stays intact.
Token replacement retains temp+sync+rename+SyncDir: interruption before rename
preserves the prior durable marker; preparation removes the temp. Hosted resume
sweeps the same v0.22 sessions/<id>/workbench-token.json. Upgrading needs no state
migration, and old/new processes exclude one another with identical lock semantics.

Run/Start racing Close recheck closing after directory validation. Close cancels
the active set, awaits settlement, closes runner/workspace, removes state and
releases the lock. Cleanup failure is CommandCleanupUnknown; state is preserved
and every Close returns the same error. A refused Linux Start does not launch.
Unknown exit or interrupted command never implies successful work or rollback.

The workspace worker's cancellation grace and stuck latch are unchanged.
Close does not claim abandoned I/O settled. Session still owns interrupted-write
record interpretation and reserved-temporary recovery without replay.

## Verification and release

Tests live beside the code they exercise; mixed suites retain hosted transcript
and session behavior in session and move profile/canary/runner assertions into
sandbox. Existing names and assertions are retained. Extra tests cover split
cache/reset behavior, every sandbox error translation, deprecated wrapper
refusals/lifecycle, empty or mismatched proof admission, and old token/lock state.
The legacy runner test adapter preserves old setup/JSON assertions without
importing session or adding a production hosted-tool dependency.

CI's existing matrix covers macOS, Linux distribution bwrap, Linux 0.8.0 and
Windows under go vet and go test -race, with sandbox-required skips forbidden.
Cross-compilation covers all Windows test packages, retaining native coverage.
The owner checks all CI jobs and tags/publishes v0.23.0 after landing.


## LAH-32 draft-1 investigation and historical validation

Source baseline: `181960cb6bcb320f89c4d71b3e157439b7441d29`, as supplied by
the planning record. No commits, dependency replacements, branch changes or
publication were made. The source candidate is identified by the SHA-256 of the
sorted manifest of Go sources, go.mod and go.sum (each line is
`<file SHA-256>  <relative path>\n`):
`1490d561fd6a2391f49df5898467000dd38a1ba0cbf157beb23fef58ca673ea3` (380 files). Documentation and workflow changes are outside
that code digest. Recompute with `rg --files -g '*.go' -g go.mod -g go.sum`,
sort paths, hash each file, then hash the resulting manifest.

### Evidence boundaries

The supplied CI report is run 37167992825, Linux/bubblewrap 0.8.0,
commit 3017616, returning `OUTSIDE-MARKER-4d1c\n` at the old regression's
line 52; main reportedly passed the same commit. Its raw CI log was not
available in this offline workspace. This is supplied evidence, not a
newly witnessed Linux execution or an established kernel mechanism.
LAH-26's stopped investigation was read; its draft was not assumed landed.

The new fixture keeps the outside source. A channel handshake joins the
workspace-name link/open/unlink swap before reading. The descriptor really
opened through that workspace name; after unlink, its facts are
`Regular=true, Directory=false, SameMount=true, Links=1`. The workspace name
really no longer exists. Instance-local stat/open adapters then **model** that
name still being reachable while returning the real descriptor and stat facts.
Production defaults are unchanged; no global hook or policy bypass is added.
The model does not pause unlink inside a kernel and does not reproduce an APFS
or Linux namespace event. It supplies an admission-rule counterexample.

The first read-path experiment returned the outside marker. It was repeated
after adding the explicit unchanged-source assertion, with only the read
policy guard temporarily removed and restored in a finally block. The other
containment changes remained present: this was a read-path counterfactual,
not a complete unrestricted baseline suite. Exact experiment inputs:

- `sandbox/workspace.go` SHA-256:
  `3a55dfd855910453b9642e999770aa6761fee1a1328bbc9b13e655a429cb1226`
- `sandbox/workbench_link_test.go` SHA-256:
  `2160a9ba9e540996e12639c5e51a6e75e59db33d505dbc508f025e4dd5fab68b`
- Command: `go test -v ./sandbox -run '^TestWorkbenchHardLinkAndConcurrentSwaps$' -timeout 30s`
- Exit 1, expected counterexample: result content
  `OUTSIDE-MARKER-4d1c\n`, IsError=false, error=nil.
  Outside source was asserted unchanged with those same bytes.
- This proves the retained admission samples can accept the modeled descriptor.
  It does not prove the modeled namespace state can occur on this filesystem.

With the policy restored, the exact same fixture receives typed `not_offered`
before either adapter is invoked. The outside-marker prohibition, ordinary
workspace target controls, persistent link facts and completed-unlink control
remain; Windows keeps real linked-file refusal and ordinary reads. The refusal
boundary tests check public and private read/search/edit against ordinary,
outside-linked, missing and malformed targets and recursive search, with open,
chunk and write-stage hooks and filesystem snapshots. They also exercise
concurrent requests, cancellation, closed admission and instance isolation.
Cancellation may win admission or an already admitted request may return the
typed refusal; edit's existing cancelled-write result mapping is retained.
No content I/O, parent creation, temporary creation or edit effects are allowed.

### Executed checks

Host: Go 1.27.1, darwin/arm64, Darwin 27.0.0
(xnu-13432.1.9~1/RELEASE_ARM64_T6000), local APFS workspace.
These are host library tests in the agent's outer filesystem/process sandbox,
not a successful nested Seatbelt command execution. macOS Seatbelt uses
`/usr/bin/sandbox-exec` shipped with this OS; a separate runtime version was
not reported. Bubblewrap is inapplicable here.

| Command | Outcome and boundary |
| --- | --- |
| `go vet ./...` | Exit 0 on macOS |
| `go test -race -count=1 ./... -timeout 180s` | Exit 0 on macOS; full package results, normal prerequisite skips permitted |
| `go test -race -count=200 -v ./sandbox -run '^TestWorkbenchHardLinkAndConcurrentSwaps$'` | Exit 0, 200 deterministic modeled fixture passes, no skips; stability, not an all-interleavings admission proof |
| `go test -race -count=1 -v ./sandbox -run '^TestContentToolsRefuseBeforeIO$'` | Exit 0, no skips; zero-content-I/O refusal evidence |
| `go test -race -v ./session -run '^TestWorkbench(DisabledContentSessionLifecycle\|HistoricalContentRecovery)$' -timeout 20s` | Exit 0, no skips; scripted Start/Open/Resume, reduced advertisement/admission, closing handler and historical recovery |
| `CGO_ENABLED=0 go build -buildvcs=false ./...` | Exit 0; CGO-free build; buildvcs disabled to avoid stat-cache writes outside the permitted workspace |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | Exit 0; all Windows tests compiled, **no Windows runtime tests executed** |
| daemon `run_check` | Exit 0, timed_out=false, stderr empty; vet/race project check in a fresh daemon sandbox; returned summary did not identify OS/runtime or enumerate skips |
| `go test -race -count=1 -v ./session -run '^TestWorkbenchMacOSEditAndRunSession$' -timeout 90s` | Prerequisite skip: outer sandbox refuses loopback bind; no command enforcement evidence |
| `go test -race -count=1 -v ./sandbox -run '^TestCommandSandboxRunSkipsRefusedCapabilities$' -timeout 90s` | Prerequisite skip: outer sandbox refuses loopback bind; no child strict-mode evidence |
| `go run -buildvcs=false ./internal/cmd/sandboxcheck` | Exit 1 before launch: TMPDIR/runtime home is inside the workspace in this outer sandbox; no contained-suite evidence |

The initial daemon copy failed on a disappearing compiler temporary; the
retried check above completed successfully. Earlier intermediate expectation
failures were fixed and are not final-candidate successes. Changes invalidate
mismatched candidate evidence; only the code digest above identifies the draft-1
code tested in those checks. These results do not validate later merged candidates.

### Draft-1 outstanding team evidence

No Linux executor or Windows runtime was accessible, and network/CI access is
disabled. Linux baseline-model, refusal, 200-repeat, vet/race and both
bubblewrap-version outcomes remain unexecuted here. macOS real command and
strict-refusal evidence was unavailable in the outer sandbox. Added focused
CI steps print source commit, Go/OS/filesystem and bubblewrap version, run the
three regressions under the race detector, and retain the existing strict
full suites and real command checks on macOS, Windows, Ubuntu/bubblewrap 0.9
and bubblewrap 0.8.0. Future CI execution is required evidence, not a pass
claimed by this change. These remain team requirements; the owner's
after-landing reruns do not replace them.

LAH-33 and LAH-34 already carry the approved consumer split and depend on
LAH-32. Their migration/validation is not claimed complete here. The library
contract they adopt is `Capabilities.WorkbenchTools`, aggregate file
capabilities unsupported, continuing API sessions, unchanged configuration
hashes and no historical replay. LAH-27 and LAH-28 already wait for this
containment repair and own reviewed storage/admission design and restoration.
Command PATH, loopback, native confinement and unrelated queued work were not
absorbed.


## LAH-32 review repair, after main afbaf07

This revision retains the production refusal and session contract and incorporates
main afbaf07 as merged by the task runner. It changes no permission defaults,
provider behavior or generated assets. No generation directives apply to these
changes. The source commit identifier supplied for the merge is afbaf07; the
exact revised source is identified independently by the manifest below.

The previous early content guards were too broad: they suppressed unaffected
directory walking, listings, listing cancellation, bounded-result assertions,
formatter controls and atomic write/symlink-swap controls. Those controls now run
on Linux/macOS. Mixed tests branch only read/search/edit expectations; remaining
whole-test guards cover content-only operations. Windows retains positive reads,
searches and edits.

Edit's direct target open and io.ReadAll now have an instance-local contentStep
observer at their actual boundaries. Refusal tests install it before admission
and assert no calls, alongside the existing read/search and write-stage hooks.
Concurrent admitted requests cover Read, Search and Edit. A local mutation
experiment removed only edit's guard, ran
`go test -count=1 -v ./sandbox -run '^TestContentToolsRefuseBeforeIO$' -timeout 30s`,
then restored the source in a finally block. Exit 1 was expected: the test
reported both `edit_file target open reached` and `edit_file target read reached`.
This is test-sensitivity evidence, not passing containment evidence. No mutation
or runtime switch remains in the candidate.

Synthetic session cases now cover default, Write, writable Commands and
read-only Commands across Start, Open and Resume (12 cases). They exercise the
merged launch-time proof orchestration with a synthetic proof callback and
runner constructor; production retains NewRunner's normal checks. Each Commands
case verifies exact effective capability names (including run_command), exact
advertisement, admitted command results, successful permitted writes when
enabled, a later closing handler after three refused content calls, fresh
proof/runner preparation on resume and no historical replay. These are
transport/contract tests, not evidence of OS command sandbox enforcement.

The modeled fixture is now shared with
`TestWorkbenchModeledReadAdmissionBaseline`. That test executes the retained
openRegular admission beneath the public content-operation refusal, reads only
the fixture descriptor, and requires the observed marker. The descriptor's
identity matches the retained outside source; regular-file, mount and link
facts are real. Namespace reachability is still modeled. Completed-unlink and
ordinary-file controls use real namespace observations. This test offers no
public/runtime bypass and makes the baseline admission observation runnable on
both Linux and macOS without changing production policy. It does not claim a
complete historical read_file invocation or exact reproduction of CI's kernel
event. Both Linux CI variants and macOS CI now run this baseline explicitly
before the containment repetitions.

### Exact revised candidate

Go source/module manifest SHA-256:
`6c28810a01a0c002537d3e402e68d3144a669e07b21dd3d96f4dde587b8a5e24` (381 files).
Algorithm: sort the paths returned by
`rg --files -g '*.go' -g go.mod -g go.sum`; for each write
`<SHA-256 of file bytes>  <path>\n`, then hash the concatenated UTF-8 manifest.
Workflow SHA-256: `f9dc0af4889cd915e959225c084e4e267f6258bb86ae675f5e3c445e217162a6`.
Documentation lies outside these source/workflow identities.

### Executed revision validation

Local platform: Go 1.27.1, darwin/arm64, Darwin 27.0.0,
xnu-13432.1.9~1/RELEASE_ARM64_T6000; APFS workspace. macOS command runtime:
OS-supplied /usr/bin/sandbox-exec; no independent version reported.
These local executions are within the implementer's outer sandbox.

| Command | Outcome |
| --- | --- |
| `go vet ./...` | Exit 0 |
| `go test -race -count=1 ./... -timeout 180s` | Exit 0; normal environment probes may skip, so this is not strict OS enforcement evidence |
| `go test -race -count=200 -v ./sandbox -run '^TestWorkbenchHardLinkAndConcurrentSwaps$'` | Exit 0, 200 passes, no skips; typed refusal and no modeled namespace observations reached |
| `go test -race -count=1 -v ./sandbox -run '^TestWorkbenchModeledReadAdmissionBaseline$\|^TestContentToolsRefuseBeforeIO$'` | Exit 0, no skips; actual baseline bytes `OUTSIDE-MARKER-4d1c\n`, unchanged outside source, Regular=true, Directory=false, SameMount=true, Links=1; all three content operations refuse without target content I/O |
| `go test -race -count=1 -v ./session -run '^TestWorkbench(DisabledContentSessionLifecycle\|HistoricalContentRecovery)$'` | Exit 0, all 12 lifecycle cases and historical recovery pass, no skips |
| `CGO_ENABLED=0 go build -buildvcs=false ./...` | Exit 0; CGO-free build |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | Exit 0; compilation only, no Windows runtime execution |
| daemon `run_check` | Exit 0, timed_out=false, stderr empty; full vet/race project check, including the newly added baseline and command lifecycle tests |

The daemon summary does not report its OS, kernel, sandbox version or skips.
It is recorded as a passing project check without inventing Linux/Windows
provenance. The added CI commands retain source commit, Go version, OS/kernel,
filesystem and bubblewrap version beside the baseline/refusal/session outputs.

### Team validation still pending

The earlier owner-step assignment of required platform checks was incorrect;
none of these team requirements has been moved to owner undertakings.
A Linux baseline and final Linux/bubblewrap 0.9 and 0.8.0 runtime results,
Windows runtime results, and strict macOS real-command enforcement have not
been returned by the available executions. Future CI steps are prepared but
are not counted as already executed evidence. run_check has no platform
selector in its exposed schema. The installed local Docker client reports
permission denied connecting to its selected Unix socket; that is an unavailable
local path, not proof that team/daemon platform executors do not exist.
The team execution entry point has been requested while all independent repairs
and checks continued. Required cross-platform evidence remains a team completion
requirement; after-landing owner reruns are still context only.

## Readable command PATH and execution

The shared runner filters the merged environment before each invocation using
the layout's existing System, Read, Work, Home and Tmp coverage. Seatbelt pairs
data-read and process-exec selectors in one emitter; metadata ancestors do not
authorize executable descendants. The Linux mount builder is unchanged.
Run, Start and low-level runners use this mechanism. Diagnostics occupy the
existing stderr budget; hosted/compatibility settlement preserves the same note.
Current proof identities are Seatbelt v11, bubblewrap v4 and readable-path-v2,
rejecting draft-4 evidence after fixture and scratch changes. PATH fixtures are
scripts; Darwin independently excludes the installed native echo tool from a
disposable read policy while retaining its interpreter as a file-only grant.
Linux checks a copied ELF echo tool outside the bind set; mounts are unchanged.
ProofError.Step and Facts.ProofStep name fixed preparation/control/launch/judgment
steps without raw diagnostics. Failed checks cache nothing and create no command
state. The scratch root is captured during preparation; fallback Mkdir uses its
anchored descriptor and checks path identity, privacy and ownership before/after.
Replaced Tmp paths refuse admission and cannot redirect host fallback writes.

Start failures after launch return a settled handle together with the error.
Result retains bounded diagnostics and the settled error; callers must inspect
non-nil handles even on failure. Pre-launch refusals retain nil handles.
Deprecated wrappers preserve post-launch handles and fixed proof-step facts. See the command design for
actual execution results and unrun checks.

## LAH-32 merged draft-3 validation

Merged main identifier supplied by the task:
`1c0700f2ab413bacab23711e9fb1abb5e8ad2514`. The documentation conflict is
resolved by retaining both the LAH-32 investigation and the landed
readable-command PATH/execution contract.

Current Go source/module manifest SHA-256: 2bc211111e5af8ead7f04f60d624203f13dba31801270db5fc58d27928d39e50 (387 files), using the algorithm above. Workflow SHA-256 remains f9dc0af4889cd915e959225c084e4e267f6258bb86ae675f5e3c445e217162a6. Draft-2 hashes and outcomes above describe that earlier candidate only.

Runtime evidence for Linux/bubblewrap 0.9.x and 0.8.0 and Windows is still unavailable through the exposed tools: run_check accepts only an empty object and returns no runner provenance; external network and CI dispatch are prohibited in this checkout. A team executor entry point has been requested again. This does not move team criteria to owner checks or establish platform success.

Re-executed against this merged source identity on Go 1.27.1 darwin/arm64,
Darwin 27.0.0 xnu-13432.1.9~1/RELEASE_ARM64_T6000, APFS. Sandbox runtime is
the OS-supplied /usr/bin/sandbox-exec, without an independent version.

| Command | Observed outcome |
| --- | --- |
| `go vet ./...` | Exit 0 |
| `go test -race -count=1 ./... -timeout 180s` | Exit 0; environment-probe skips remain possible, not strict enforcement evidence |
| `go test -race -count=200 ./sandbox -run '^TestWorkbenchHardLinkAndConcurrentSwaps$'` | Exit 0, 200 passes, no skips |
| `go test -race -count=1 -v ./sandbox -run '^TestWorkbenchModeledReadAdmissionBaseline$\|^TestContentToolsRefuseBeforeIO$'` | Exit 0, no skips; baseline marker bytes, unchanged source, Regular=true, Directory=false, SameMount=true, Links=1; refusal coverage passes |
| `go test -race -count=1 -v ./session -run '^TestWorkbench(DisabledContentSessionLifecycle\|HistoricalContentRecovery)$'` | Exit 0; all 12 synthetic lifecycle cases and historical recovery pass, no skips |
| `CGO_ENABLED=0 go build -buildvcs=false ./...` | Exit 0 |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | Exit 0; compilation only |
| daemon `run_check` on merged source | Exit 0, timed_out=false, stderr empty; sandbox 6.697s, session 123.634s; runner platform and skips unspecified |
| `AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -count=1 -v ./sandbox -run '^TestCommandSandboxRunSkipsRefusedCapabilities$'` | Exit 1 at prerequisite: loopback listen bind operation not permitted; no command enforcement observation or passing skip |

Strict macOS command enforcement therefore remains unavailable locally, rather
than passing. No production changes, generated-file changes, dependency
replacements or consumer edits were needed to combine the landed command policy
with this repair.

Draft-4 validation follow-up: the source and workflow identities above were
recomputed and remain unchanged. The exposed tool inventory still provides only
`run_check({})` for daemon execution, without platform selection, command override
or runner provenance. A shared task note now requests the team runner entry point
or exact-candidate CI logs for the outstanding platforms and strict enforcement
checks. This is an unresolved team execution dependency; no new passing platform
claim or after-landing owner requirement is introduced.
The repeated daemon project check completed with exit 0, timed_out=false and
empty stderr (sandbox 11.362s, session 124.479s). As before, the summary contains
no platform/sandbox provenance and does not satisfy the missing runtime evidence.

## LAH-32 draft-5: supplied platform evidence and crash-fixture repair

The owner proxy supplied results for draft-4 commit
`1eb1bf9404359e4522950bcd429036b1e8c5b60c`, executed outside the sandbox:

- macOS 27 arm64: `go vet ./...`, the strict race suite with
  `AGENT_HARNESS_TEST_NO_SKIP=1`, and `go run ./internal/cmd/sandboxcheck` passed.
- Windows: `GOOS=windows go vet ./...` and test compilation of sandbox/session/
  process passed. The owner explicitly accepts Windows runtime CI on main after
  landing; compilation is still not recorded as runtime execution.
- Ubuntu 24.04, bubblewrap 0.9.0, strict suite with bind-mount witness: failed
  `TestWorkbenchLinuxLaunchFailureAfterProof` and
  `TestStartupBeforeNotifyRetainsDiagnostics/true`. The owner identified these
  as inherited base-1c0700f failures, fixed upstream in dea80b0. The supplied
  main dea80b0 CI result is green across all four jobs, including bubblewrap
  0.8.0; it is evidence for main, not this containment candidate.
- Linux sandboxcheck failed once in
  `TestAPISessionResumeAfterACrashNeverRerunsACall` with an unscripted third
  request; the isolated test subsequently passed three times.

Main dea80b0 was merged into this checkout by the task runner without conflicts;
its startup classification and status-writer fixture fixes are retained. No Git
metadata was modified by the implementer.

The crash test's previous deferred close(release) ran before startAPI's registered
test cleanup closed the original session. Its blocked handler could then return
success to a live original turn, causing that turn's next model request to race
teardown. Workbench is nil in this test, so availability reporting and the absence
notice do not execute; they introduce no provider probe or retry. The fixture now
closes the original session before releasing the handler, awaits actual state
release, and asserts exactly two model requests. The other release-channel tests
already interrupt or close their session before releasing an admitted handler.

New candidate Go source/module manifest SHA-256:
`1d0fee3c5d7fd08248adb5e7b381b275ae9d51e591bb235f6078d69ae9faaf20`
(387 files), using the manifest algorithm above. Earlier candidate results stay
tied to their original identities. Passing focused Linux baseline, 200-repeat
containment, refusal and lifecycle results for both bubblewrap versions have not
yet been supplied; the owner has undertaken a Linux rerun on this repaired
candidate. Those outcomes are not inferred from main's green CI.

Local repaired-candidate validation: Go 1.27.1, darwin/arm64, Darwin 27.0.0,
APFS; OS-supplied sandbox-exec (no separate version). Commands and outcomes:

The repaired candidate's daemon `run_check` also passed: exit 0,
timed_out=false, stderr empty, sandbox 11.739s and session 135.196s. Its platform
and sandbox provenance remain unspecified, so it is project-check evidence only.

| Command | Outcome |
| --- | --- |
| `go vet ./...` | Exit 0 |
| `go test -race ./... -timeout 180s` | Exit 0; environment-probe skips possible |
| `go test -race -count=200 ./session -run '^TestAPISessionResumeAfterACrashNeverRerunsACall$' -timeout 180s` | Exit 0; 200 passes, no skips, exactly two requests per fixture |
| `go test -race -count=200 ./sandbox -run '^TestWorkbenchHardLinkAndConcurrentSwaps$'` | Exit 0; 200 passes, no skips |
| `go test -race -count=1 -v ./sandbox -run '^TestWorkbenchModeledReadAdmissionBaseline$\|^TestContentToolsRefuseBeforeIO$'` | Exit 0, no skips; modeled marker baseline with unchanged source and real Regular=true, Directory=false, SameMount=true, Links=1; zero-I/O refusal passes |
| `go test -race -count=1 ./session -run '^TestWorkbench(DisabledContentSessionLifecycle\|HistoricalContentRecovery)$'` | Exit 0, no skips; synthetic contract evidence |
| `CGO_ENABLED=0 go build -buildvcs=false ./...` | Exit 0 |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | Exit 0; compilation only |

## LAH-32 draft-6: exact-candidate owner results

Owner note 8 supplies outside-sandbox results for draft-5 commit
`65d291574027c030aa34daf818287a0668af3b9f`, based on dea80b0. Its Go source
manifest remains `1d0fee3c5d7fd08248adb5e7b381b275ae9d51e591bb235f6078d69ae9faaf20`;
recomputed in this round, it is unchanged. This round changes documentation only.
The following results supersede the earlier pending statements for this source,
without changing the historical evidence for older drafts:

| Platform and sandbox | Supplied command/check | Supplied outcome |
| --- | --- | --- |
| Ubuntu 24.04, bubblewrap 0.9.0, `AGENT_HARNESS_TEST_NO_SKIP=1`, bind-mount witness | `go vet ./...` and `go test -race ./...` | PASS |
| Same Linux configuration | Focused `-run 'TestWorkbenchHardLinkAndConcurrentSwaps\|Refus\|Disabled'` on sandbox and session | 162 passing tests/subtests, none failing |
| Same Linux configuration | `go run ./internal/cmd/sandboxcheck` | PASS |
| macOS 27, OS Seatbelt | Strict race suite | PASS on this exact draft-5 candidate |

These are supplied owner observations, not executions by the implementer.
The strict Linux full suite includes the modeled admission baseline and rejects
environment-prerequisite skips. The focused filter covers containment, refusal
and disabled-session cases. The note does not supply verbose baseline bytes,
descriptor facts, Go/kernel/architecture/filesystem metadata, exact focused
flags or an explicit Linux `-count=200` result; those details are not invented.
The baseline test's modeled namespace remains distinct from the original CI
event; suite success is not an exact kernel reproduction.

The owner explicitly states that bubblewrap 0.8.0 and Windows results come from
CI after landing. Those runtime checks are accepted follow-up validation for
this task, not already observed passing evidence. The recorded after-landing
Linux 0.8/0.9 rerun remains an owner undertaking. Local macOS 200-repeat
containment and crash-fixture results remain as recorded above; an explicit
Linux 200-repeat result has not been supplied.

This documentation round's `run_check` completed successfully: exit 0,
timed_out=false, stderr empty, sandbox 9.430s and session 130.391s. Its summary
does not identify the platform and adds no new platform-specific claim.
