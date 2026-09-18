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

### 2. Explicit host isolation

`Options.Isolation` states, per session, whether inherited host customization is
permitted. The default is fully isolated and it is the only value an application
can rely on to be enforced:

- Claude: no setting sources, hooks disabled, strict MCP configuration with only
  the configured server, slash commands disabled, browser integration off, and
  an explicit tool allowlist.
- Codex: the configured home only (already a per-session, non-global selection),
  the configured sandbox and approval policy, and only the configured MCP
  server.

Widening this is an explicit, named choice. There is no mode that silently
inherits the operator's environment, and there is no permission-bypass setting.

### 3. Verified capability, failing closed

Where a provider reports what it actually enabled, the library checks it instead
of trusting the flag it passed. Claude's initialization frame advertises its tool
set; if that set is not the one the session configured, startup fails with
`ErrUnexpectedCapability` and the process tree is stopped. Where a provider does
not report it, the capability is recorded as `Unknown` with the reason, and the
caller can see that it is unverified rather than being told it is enforced.

Codex's built-in tools cannot be disabled by configuration. The library says so
through `Capabilities.RestrictTools` (`Unsupported`, with reason) rather than
implying an enforcement it does not have. Codex's own sandbox and approval
policy still apply and are still explicitly configured.

### 4. Health that distinguishes states

`Session.Health()` reports `Running`, `Active`, `Quiet`, `Exited` or `Failed`,
with the time of the last observed event and the number of tool calls in flight.
Quiet means no event recently and the process is alive: that is unknown, not
stuck, and the library draws no conclusion from it. Only process exit and an
explicit provider failure are terminal.

## What stays out

Budgets, thresholds, retry policy, scheduling, prompts, persistence policy and
authorization remain the application's. The library reports observations and
offers hooks; it does not decide when a session should stop working.

`completion` is unchanged. A native tool-enabled session and a constrained
tools-disabled completion remain two different execution contracts, and one is
still not a substitute for the other.

## Testing

Synthetic CLI fixtures and fake transports only: MCP framing, tool dispatch,
oversized and unknown calls, isolation argument construction per engine,
advertised-capability mismatch, health transitions, and bridge listener
lifetime. No real models, accounts, credentials or external mutations.
