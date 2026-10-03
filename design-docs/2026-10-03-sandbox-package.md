# The sandbox package: stage A

The OS sandbox's security-critical code needs a boundary that can be reviewed
apart from session orchestration, tool admission, telemetry and transport.
This stage introduces `github.com/shhac/lib-agent-harness/sandbox` within the
existing module. A separate module is out of scope.

## Boundary in this stage

`sandbox.Workspace` owns the workbench's confined file reads, listing, search,
atomic writes and edits, reserved names, link and mount checks, directory
walks, platform name and durability rules, and single I/O worker. `OpenWorkspace(Config)`
opens its root. Config carries the root, effective new-file permissions,
result budget, session ID, cancellation grace and `OnFailure` callback. A zero budget uses 64 KiB;
a zero grace uses ten seconds. Session continues to normalize the workspace,
permissions and tool budget before opening it. These low-level options are
caller-controlled; Workspace does not provide session authorization policy.

`Read`, `List`, `Search`, `Write` and `Edit` accept the existing workbench JSON
arguments and return `Result{Content, IsError}` plus an error. `Definitions`
provides the same descriptions and schemas; its write flag selects which
schemas to advertise, rather than authorizing filesystem operations.
`ResolveName` and `CheckDir` let the command runner validate its directory
without reaching into workspace handles. `RemoveReserved` removes only a
session's reserved temporaries beside an interrupted write target. Directory
validation and recovery are lifecycle operations: callers must settle file
calls before recovery. `SyncDir` shares the platform durability rule with
command-state preparation, which remains in session.

Shared containment comparisons are `sandbox.Nested` (including ancestor
identity checks) and `sandbox.Within` (lexical comparison). Session keeps tiny
adapters for the command code that moves in stage B. Read-directory
normalization and platform data aliases remain there because the file
implementation does not need them. UTF-8 result truncation is shared through
`internal/textbound`, rather than duplicated across the packages.

Session's `workbenchHost` holds files, the command runner and session metadata.
Its file handler only calls Workspace, converts Result to ToolResult and
translates errors. Transcript interpretation and unknown-outcome accounting
stay in session. Fault injection and worker observations shared by in-module
tests use `internal/sandboxhook`; no test controls become public API. Session
knows the ID before opening the workspace: it generates a new reference
or uses the resumed reference's ID and passes `Config.SessionID`. It also passes
`Config.OnFailure`, a closure that translates and latches the failure and forwards
it to the session once bound. Production session code never uses the test bridge;
the bridge exposes only fault injection and observations, not configuration.

## Error translation and lifecycle

Sandbox owns `ErrUnsupported`, `RefusalError`, `ProofError` and `CommandError`.
They publish the same fixed harness facts: OpenAI-compatible engine, session
operation, preflight or capability refusal families, before-launch proof
phase and turn failure family. Bubblewrap's actionable error messages are
preserved. Only workspace setup refusals are emitted here in stage A. The retained
command refusal codes prepare stage B: `RefusedRuntimeHome` describes unusable
private recovery storage, `RefusedSandboxRead` an invalid command read set,
`RefusedLimit` command option bounds, `RefusedConflict` incompatible command
settings, and `RefusedNotOffered` an unsupported command operation. Native-only
`RefusedEnvManaged` and native-probe codes remain in session. Only conflict and
not-offered refusals use the capability family; the others are preflight errors.
The proof codes describe missing/outdated tools, unavailable namespaces, a proof
that cannot run or times out, and escaped canaries. `ErrCommandFailed` identifies
the currently emitted workspace settlement failure and future command failures,
retaining its legacy turn-failure wording. Session aliases shared constants to
sandbox constants so the code sets cannot drift. File-tool `ArgumentsInvalid`
is a result code, outside the turn-failure translation table.

Session's single `fromSandbox` boundary maps them to the existing
UnsupportedError, CapabilityError and TurnError, retaining session sentinel
identity. Context errors and unrelated errors pass through; the sandbox
closed sentinel maps to session.ErrClosed. Unknown write outcomes keep the
same sentinel used by session's accounting.

The worker's channel, cancellation grace and stuck latch retain their
settlement rules. A late `workspace_io_stuck` is translated before invoking
session failure handling, so health, admission, control and Release still
observe the typed turn failure. Close does not assert that abandoned I/O has
settled. Recovery uses the existing unanswered/unknown write records and does
not replay them. A reserved-temporary removal failure still makes resume
return StateUnusable. Other sessions' temporaries remain untouched.

Open order remains proof, workspace, transcript, command preparation, cleanup.
Early refusals close the workspace and any prepared command runner. Session
shutdown interprets cleanup records, closes commands, then closes file access;
command-close uncertainty is reported through the failure callback. Recovery
record append failure now also closes a prepared command runner, releases the
transcript and workspace, and removes a fresh session directory. This closes an
error-path resource leak found during extraction; successful behavior is unchanged.
A fixture fails the actual transcript write and checks close order, lock release,
workspace admission closure and fresh-state removal versus resumed-state retention.

## What stays, and why

`process` remains a public top-level package: it contains native CLI harness
processes as well as sandbox commands, and is shared with account/Grok
execution. It is not an implementation detail of this boundary.

`internal/wsfile` stays where it is: internal/skills shares its path, reserved
name and opened-handle checks. Nesting it under sandbox would hide that shared
mechanism from skills. `session.Sandbox`, the native Codex/Claude CLI sandbox
configuration and its probes remain in session; they are distinct from the
workbench's application-controlled file tools.

Stage B moves the Seatbelt and bubblewrap profiles, canaries, proofs and cache,
runner and supervisor, command state and standalone command API. It introduces
the standalone `sandbox.Open(ctx, Options)` API, completes the README package
structure and this design, and releases the next minor after the version
current when it lands. Stage A's Workspace API is provisional;
`OpenWorkspace` remains distinct from stage B's command opener. v0.22.0 belongs
to the standalone command work; no release is cut here. Consumer command-sandbox adoption waits for stage B.

## Compatibility evidence

Session's Workbench, Commands, OpenCommandSandbox, error types and codes and
WorkspaceIOStuck are unchanged. No harness.Support claim changes. Model-facing
text, schemas, bounds, reserved names, mount and link policy, atomicity,
timeouts, Ref digests and workbenchDigest remain unchanged. State paths and
`.harness-workbench-<ID without dashes>-<hex>.tmp` names remain byte-identical;
old binaries' temporaries are removed through the same recovery policy.

Proof functions stay in session. OS-specific pinning tests reconstruct the
existing payloads with literal `seatbelt-workbench-v4`, `bwrap-workbench-v2`
and `sessions/id/workbench/{home,tmp}` shapes. The Seatbelt profile has a literal
SHA-256 pin for a fixed layout and system list. The bwrap flags and mount argument
slice have a literal fixture; host-dependent ancestors are rebuilt independently.
Linux payload reconstruction uses a frozen argument builder, system set and path
comparison instead of live production helpers. They move with their functions
in stage B; a failure must be fixed, never re-pinned. Cached keys must remain
valid across both extraction stages.

Pure file suites move beside the sandbox implementation. Mixed write and
worker suites split by test function; names are retained. Command, bwrap,
adversary, environment and standalone suites remain in session. Additional
checks cover the import boundary, every sandbox error code's translation,
public serialized calls and stuck I/O, old-format transcript recovery and
forced removal failure. CI already tests all packages on macOS, Linux with
both distribution bubblewrap and 0.8.0, and Windows; no matrix change is needed.
