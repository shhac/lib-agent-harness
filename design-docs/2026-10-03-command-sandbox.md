# Standalone command sandbox

The session package exposes OpenCommandSandbox without a model session. Its
options are WorkDir, RuntimeHome, Write, Read, Env, Loopback, Timeout and
Background. Run takes CommandRequest{Command, Dir, Timeout} and returns a
CommandResult{ExitCode, Stdout, Stderr, TimedOut, Truncated}. Start takes the
same request with no Timeout and returns StartedCommand with Stop, Done and
Result. Start reports process launch rather than application readiness.

## One implementation and proof

Both standalone execution and Workbench.Commands use the internal
commandSandbox, configured by normalized Options and the proved layout,
environment, token, binary identity and output budget. Its execute function
returns a typed result; run maps it back to the hosted tool's byte-compatible
JSON and existing error contract. There are no model prompts, credentials or
API calls in opening or running standalone commands.

Open uses normalizeRuntimeHome, normalizeWorkbench, proveWorkbench and
openWorkspace. No command state exists until proof succeeds. Refusals are the
same UnsupportedError and CapabilityError as the workbench. A failed or timed
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
access must still fail. The Seatbelt proof version is bumped, so prior evidence
cannot stand in for the inbound check. Started servers are reachable from the
host; without Loopback, both serving and connecting are refused.

Linux always creates a private network namespace with an active lo. A command
always has its own private localhost and never the host's. Loopback on Run
requires an additional own-loopback bind/connect canary. The existing host
listener and off-machine checks remain. The own-loopback canary requires
OpenBSD nc (for example the netcat-openbsd package); traditional and BusyBox
nc use incompatible listener syntax and are not supported. The bubblewrap proof version is
bumped and Loopback joins the key. No seccomp layer is added: the owner accepted
private lo even without Loopback, since it grants nothing outside the sandbox.

Start with Loopback on Linux refuses before launch with UnsupportedError,
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
RuntimeHome/commands/<random id>/, with an exclusive lifetime flock. A short
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
with distribution bubblewrap and 0.8.0. This additive API is planned for
v0.22.0, following the browser bridge home API in v0.21.0; consumer adoption
is separate and requires no breaking migration.
