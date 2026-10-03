# Session tool hosting and host isolation

Dated 2026-09-18. Extends `session` so an application can run a persistent
native coding agent whose tools it owns and executes itself.

## Problem

`session` could already open, resume, steer, interrupt and observe a persistent
native CLI conversation. What it could not do was give that conversation a tool
set the *caller* implements. A caller therefore had two options: let the CLI use
its own tools against the host, or fall back to `completion`, where tools are
proposals the application executes between stateless invocations. Neither suits
an application that owns an isolated execution environment the CLI itself cannot
reach.

## What the library adds

### 1. Caller-hosted tools

`Options.Tools` describes an MCP tool host:

```go
session.ToolHost{
    Server:  "workspace",            // server name; tool ids become mcp__workspace__*
    Bridge:  session.Bridge{Path: exe, Args: []string{"tool-bridge"}},
    Tools:   []session.ToolDefinition{...},
    Handler: session.ToolHandlerFunc(...),
}
```

The library:

- serves the MCP protocol itself (`initialize`, `tools/list`, `tools/call`) over
  a private listener created in a caller-supplied directory;
- writes the provider-specific launch configuration that points the CLI at that
  listener through the caller's bridge command;
- reports each tool call to the caller's handler, with the turn it belongs to,
  and emits `tool_started`/`tool_completed` events for it;
- bounds request and response sizes and refuses unknown tool names.

The bridge is a caller-supplied command because only the application knows which
binary it can re-execute. Its job is fixed and policy-free: relay the CLI's MCP
stdio to the library's listener. The library does not spawn shells, does not
search `PATH` for a bridge and does not invent one.

Tool *execution* is the caller's: the library never touches the filesystem,
network or containers on a tool's behalf.

### 2. The restricted session contract, opt-in

`Options.Restriction` opts one session into a stricter contract than an ordinary
native session: the CLI's own tools are removed, inherited host customization is
disabled, and the caller's tool host becomes the session's entire tool surface.
It is **opt-in**. A session opened without it behaves exactly as it did before,
so existing callers are unaffected and no global default changes underneath
them.

Both engines have a real implementation. The Codex one reuses what constrained
completion already does: the model catalog entry for the selected model is
rewritten to disable the shell type, drop the apply-patch tool, empty the
experimental tool set and pin standard tool mode, alongside the feature switches
that turn off shell, unified exec, apps, plugins, hooks, subagents, browser,
computer use and image surfaces, with user config, rules and project documents
ignored. That logic moves into a shared internal package so `completion` and
`session` cannot drift apart. The Claude one passes an explicit allowlist
holding only the caller's MCP tool identifiers.

A sandbox mode that forbids writes is not part of this argument. Restricting
what a tool may *write* does not restrict what it may *read*, and a host-resident
CLI that can read arbitrary files can disclose credentials and private data
through ordinary inference. The restriction is the removal of the tools.

### 3. Capability proved before the credentialed process starts

Trusting a flag is not evidence, and reading a tool catalog the provider reports
*after* a session is running is too late: by then the process exists, holds the
caller's login and could already have acted. So `session` reuses the completion
probe design and runs it **before** the real launch.

The probe starts the same binary with the same restricted arguments, but with a
disposable home, a dummy credential and a loopback endpoint that rejects every
request, and drives one synthetic turn. The caller's tool host is attached, so
the CLI negotiates the real tool surface, but tool *calls* are refused for the
probe's lifetime. The rejected request body is then checked for exact agreement:
the configured tools must all be present, nothing else may be, and no inherited
instruction material may have been merged. A rejecting provider performs no
inference, so no agent loop and no tool call can occur; the probe process holds
no credential it could disclose.

Only a passing probe leads to a credentialed launch. A failure returns
`*CapabilityError` — engine, a fixed reason code, and the offending or missing
tool names — and nothing is started. Where a provider also advertises its tool
set at initialization, the same comparison runs again before the first prompt,
so a mismatch still precedes any inference; where it does not, the second check
is recorded as `Unknown` rather than described as enforcement.

### 4. Health that distinguishes states

`Session.Health()` reports `Running`, `Active`, `Quiet`, `Exited` or `Failed`,
with the time of the last observed event and the number of tool calls in flight.
Quiet means no event recently and the process is alive: that is unknown, not
stuck, and the library draws no conclusion from it. Only process exit and an
explicit provider failure are terminal. Health describes a process. It never
means a task finished: only a caller-hosted tool call can say that.

### 5. Closing the tool channel atomically

A native turn may issue several tool calls concurrently. A handler can therefore
return `ToolResult{Closes: true}`, which latches the channel shut inside that
call: every later `tools/call`, including one already in flight, is refused with
an explicit closed-channel result and never reaches the handler. The caller then
decides what to do — typically interrupt and checkpoint. The library does not
infer that a turn is over from text, status or silence.

### 6. Ownership and orphan reclamation

A native CLI outlives the process that launched it if that process dies. The
library already contains the CLI in its own process group; this adds the ability
to find and end an orphaned group after a restart.

The tool bridge is the anchor, because it is the caller's own binary running as
a child of the CLI for as long as the session lives. `session.RunBridge` takes an
exclusive lock for its lifetime and records the group it belongs to in the lock
file. `session.Reclaim` observes that lock: free means nothing survived; held
means a subtree is still alive, and the group named by its current holder is
terminated and the lock re-checked. A group that cannot be confirmed gone is
reported as unresolved rather than as clean, so the caller can hold the work for
inspection instead of starting a second worker.

Tool execution stops on its own in this situation, since every call has to reach
the caller's listener to do anything. Reclamation is about the CLI process
itself, which otherwise keeps its provider connection and keeps spending.

### 7. Truthful streaming and failure diagnostics

Usage is emitted as it is observed, not only at the terminal result: each model
response's provider-reported figures produce a `usage` event marked as a
per-request observation, and the terminal accounting is marked as such. A failed
or interrupted turn keeps its observed figures as evidence in `Result.Observed`
while `Result.Usage` stays unknown, so a caller can show what was seen without
recording an unmeasured turn as free. Nothing is summed across turns.

Process failures stop collapsing into one opaque transport error. The transport
retains a bounded tail of the CLI's standard error and reports a typed
`*ProcessError` carrying the exit status and a fixed reason code. Following the
rule the library already applies to provider text, that captured output is never
placed in an error string or a public field; it is offered once, sanitized and
bounded, to an optional `Options.OnDiagnostic` hook, so a caller can record it in
its own private diagnostics without it reaching a user interface or a log by
default.

## What stays out

Budgets, thresholds, retry policy, scheduling, prompts, persistence policy and
authorization remain the application's. The library reports observations and
offers hooks; it does not decide when a session should stop working.

`completion` is unchanged. A native tool-enabled session and a constrained
tools-disabled completion remain two different execution contracts, and one is
still not a substitute for the other.

## Testing

Synthetic CLI fixtures and fake transports only: MCP framing, tool dispatch,
oversized and unknown calls, concurrent calls after the channel closes,
restricted argument and catalog construction per engine, probe acceptance and
every rejection reason, advertised-capability mismatch, reference stability
across a changed listener path and bridge credential, health transitions,
bridge lock ownership and reclamation, per-request usage on successful and
failed turns, and typed process-exit diagnostics. No real models, accounts,
credentials or external mutations.
