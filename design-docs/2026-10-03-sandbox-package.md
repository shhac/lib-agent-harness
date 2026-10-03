# The sandbox package

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

Proof-key payloads are byte-identical: `seatbelt-workbench-v4`,
`bwrap-workbench-v2`, JSON field order and types, Env/Background, the fixed
Seatbelt template and bwrap argument builder, and `sessions/id/workbench/home`
and `sessions/id/workbench/tmp` shapes. Even standalone proofs keep the historical
session shapes. The moved stage-A tests reconstruct the frozen payloads,
Seatbelt SHA-256 and bwrap argument fixture; their literal pins are unchanged.

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
