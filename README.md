# lib-agent-harness

Go interfaces for installed Codex and Claude CLI harnesses: constrained model
completion, native agent runs, model discovery, streaming, session control,
and account/quota/context telemetry.
The selected engine changes configuration, not the calling interface.

The library launches local CLI binaries and uses their native login. It does
not require a hosted execution service or a direct model API key. The CLI's own
account and billing rules still apply.

```sh
go get github.com/shhac/lib-agent-harness
```

Requires Go 1.26.4 and the selected CLI installed locally. macOS, Linux, and
Windows are supported. CLI protocols evolve: pin and test your deployed CLI
versions, and handle unsupported capabilities at runtime.

## Choose the execution contract

| Package | Contract |
| --- | --- |
| `completion` | Model returns text and proposed application tool calls. Native tools are disabled and verified before inference; the application authorizes and executes proposals. |
| `native` | One native agent invocation, optionally resuming a session. Generic structured output, tool activity, readable transcript, and usage parsing. |
| `session` | Persistent bidirectional sessions with turns, streaming events, interruption, resumption, and capability-aware steering. |
| `process` | Shared subprocess-tree containment, including Windows suspended-start job assignment. |

These are explicit execution modes. A native agent session must not substitute
for constrained completion when the application relies on native tools being
unavailable. Applications own prompts, orchestration, scheduling, tool
permissions, budgets, durable state, and retry decisions.

## Constrained completion

```go
reply, usage, err := completion.Complete(ctx, completion.Config{
    Engine: "claude", // or "codex"
    Model: "haiku",   // discover the installed CLI's models; no library model default
    Effort: "low",
}, []completion.Message{
    {Role: "system", Content: "Answer briefly."},
    {Role: "user", Content: "Suggest a short progress caption."},
}, nil)
```

The message roles above are part of the application conversation presented to
the constrained model, not a promise that every provider accepts identical
native system-message operations. See [completion](completion/README.md) for
the stronger execution boundary, CLI compatibility probes, and scratch storage.

Use `completion.DiscoverModels(ctx, cfg)` to retrieve model IDs and advertised
efforts without inference. Unsupported or failed discovery never invents a
catalog or silently substitutes a model.

## Native sessions

```go
s, err := session.Start(ctx, session.Options{
    Engine: session.Claude, // or session.Codex
    Model: "haiku",
    Effort: "low",
    WorkDir: workspace,
})
if err != nil { return err }
defer s.Close()

turn, err := s.StartTurn(ctx, session.Input{Text: "Summarize this project."})
if err != nil { return err }
for event := range turn.Events() {
    // Consume promptly; events are bounded. Apply your UI's privacy policy.
    show(event)
}
result, err := turn.Wait(ctx)
```

Native sessions use explicit provider execution policies. Default policy is
conservative but does not disable all installed customizations. Instructions
have explicit replacement/append semantics, and resume references bind the
session to its configured home, working directory, and policy. Keep references
in private application state; local paths are not public metadata. Account
identity labels are caller assertions, not cryptographic account verification.

Consume turn events while the turn runs. `Wait` does not drain the stream;
backpressure fails explicitly instead of silently dropping tool activity.
Cancellation of a wait only stops waiting. Interruption and session closure are
separate operations.

`Health` reports `running`, `active`, `quiet`, `idle`, `exited` or `failed` from
what has been observed, without contacting the CLI. Quiet means nothing recently
and the process is alive: that is unknown, not stuck, and the library draws no
conclusion from it. Health describes a process and never means a task finished.

Usage is published as it is observed. A turn makes many model requests, so each
response's reported figures arrive as a `usage` event with `Final` false, and the
turn's own accounting arrives with `Final` true. `Result.Observed` keeps the
per-response figures even when a turn fails or is interrupted, where a provider's
terminal counters are often absent or belong to an earlier turn; it is evidence
of what was seen, not a measurement of the turn. Nothing is summed across turns.

A lost CLI reports `*ProcessError` with its exit status and a fixed reason code,
still wrapping `ErrTransport`. Captured standard error never enters an error
value; set `OnDiagnostic` to receive a bounded, control-stripped, credential-
redacted tail for your own private records.

## Restricted sessions with caller-hosted tools

`Options.Restriction` opts one session into a stricter contract: the harness's
own tools are removed, inherited customization is disabled, and your tools
become the session's entire surface. Sessions opened without it are unchanged.

```go
s, err := session.Start(ctx, session.Options{
    Engine: session.Claude,
    Model:  "haiku",
    Instructions: session.Instructions{Mode: session.Append, Text: scopedTask},
    Restriction: &session.Restriction{Tools: session.ToolHost{
        Server:  "agent_workspace",           // some names are reserved by the harness
        Dir:     runPrivateDir,                  // owner-only, in your own state
        Bridge:  session.Bridge{Path: exe, Args: []string{"tool-bridge"}},
        Tools:   []session.ToolDefinition{{Name: "read_file", Schema: schema}, {Name: "finish", Schema: schema, Closing: true}},
        Handler: yourExecutor,                   // you execute; the library never does
    }},
})
```

The restriction is proved before your login is ever used, and there is no option
to skip that. The library launches the same binary with *the same arguments the
real session will use*, differing only in a disposable home, a dummy credential
and a loopback provider that refuses every request; it drives one synthetic turn
and reads what the harness actually sent. Equivalence matters: a check run with a
different permission mode or different instructions would establish something
about a configuration nobody is going to launch. A rejecting provider performs no
inference, so no agent loop and no tool call can happen during the check, and the
tool host refuses calls for its duration regardless. The check ends shortly after
the harness's first request rather than sitting out its timeout.

The judgement is in two parts, because installed harnesses differ in ways that
were measured rather than assumed:

- **Nothing unauthorized, anywhere.** No request may carry a tool outside your
  hosted set. A harness makes auxiliary requests — naming the session, for one —
  and those carry no tools at all; requiring every request to carry your set
  would reject a correctly restricted session, while a request carrying a
  built-in is refused wherever it appears.
- **Your tools, positively proven.** Usually a request carries them. Where a
  harness defers MCP tools behind a discovery tool and never puts them in a
  request at all, the proof is the tool channel's own record of having served
  them, which is direct evidence that the surface loaded and was reachable.

`session.VerifyRestriction(ctx, options)` runs exactly this check without opening
a session, so an application can tell an operator that their installed CLI cannot
be restricted while they are configuring it, rather than when work is
commissioned.

A failure returns `*CapabilityError` with a fixed reason code, the disagreeing
tool names and the phase it happened in, and nothing is launched. Where a harness
also advertises its tools at startup, the same comparison runs again before the
first prompt; that one reports `BeforeFirstPrompt`, and says the session was
closed rather than claiming it never started.

Repeating the check is avoided only by a process-local record of the same binary
— by path, size and modification time — with the same arguments and the same
tool identifiers. That is evidence about the thing the probe proved, not a
caller's assertion that evidence was unnecessary. Nothing is persisted: a
restart re-proves, which is exactly when an installed CLI is most likely to have
changed underneath.

Restricting writes is not what this does. A tool that can read is a disclosure
path whatever it may write, so the restriction is the removal of the tools; a
sandbox mode and a working directory are neither claimed nor relied on as one.

Your bridge command is your own binary re-executed as the harness's tool server.
Its whole implementation is `session.RunBridge(ctx, os.Stdin, os.Stdout)`, which
relays the protocol and holds a lock naming the launch it belongs to. The
listener lives in a short owner-only runtime directory, because an application
state path is longer than a local socket address may be; the durable files — the
channel credential and that lock — stay in the `Dir` you supplied, which is where
recovery looks for them. The credential sits in an owner-only file and never
appears in arguments, environment values, references or tool results. `Dir` also
carries an assignment lease, taken before anything is launched, so a second
process cannot drive the same assignment even before a bridge exists.

Tool calls execute one at a time. A native turn will ask for several at once,
and running them concurrently would make "after the work was reported" an
ambiguous claim — a write or a test could still be in flight when a closing tool
decides the assignment is finished, and cancelling it afterwards does not undo
it. A tool declared `Closing`, or a result with `Closes` set, therefore runs with
nothing else in flight and latches the channel only if it actually succeeded: a
finish whose arguments the handler rejected has not finished anything.
Everything queued behind it is refused without executing. `Session.ToolsClosed`
reports that state.

If the launching process dies, the harness does not: it keeps its provider
connection and keeps spending. `session.Reclaim(ctx, dir)` answers two separate
questions and never confuses them. Whether anything survives is answered by the
recorded process group, because a free bridge lock proves only that no bridge is
running — a harness can outlive its tool server, restart it, or sit in inference
with none running. Whether that group is *yours* is answered by a live bridge
naming the same launch, because process and group identifiers are reused and a
stored integer is never grounds for signalling. Only then is the group
terminated, and only a group that has actually become empty is reported as
`Confirmed`. Anything else is `ErrUnreclaimed`: hold the work for inspection
rather than start a second one.

Per-engine, the restriction is built from provider mechanics rather than from a
permission setting, and each part of it was checked against an installed CLI.

**Claude** is launched with no setting sources, hooks disabled, a strict MCP
configuration holding only your server, slash commands disabled, an empty
built-in tool list and an explicit allowance for your tool identifiers. Its
initialization then reports `tools: ["mcp__<server>__<tool>"]` and no built-ins.
Some server names are reserved: a reserved one is accepted and then silently not
loaded, leaving a session with no tools while reporting success, so the library
refuses those names up front and separately verifies from the initialization
frame that your server actually connected.

**Codex** is launched with the shared restricted model catalog — shell type
disabled, apply-patch dropped, an empty experimental tool set — alongside the
feature switches that turn off apps, plugins, hooks, subagents, browser, computer
use, image and goal surfaces. The catalog is the load-bearing part: the feature
switches alone leave `exec` and `wait` in place. Overrides are dotted-key TOML,
which is what Codex parses.

Two things about Codex are worth stating plainly rather than implying otherwise.
Its remaining surface includes MCP-mediated helpers (`tool_search`,
`list_mcp_resources`, `list_mcp_resource_templates`, `read_mcp_resource`); they
are permitted because they can address nothing but configured MCP servers, a
restricted session configures exactly one, and this library's tool host answers
every resource method with method-not-supported. And `app-server` has no
`--ignore-user-config` or `--ignore-rules` — those belong to `exec`, and
overriding the server table with `-c mcp_servers={}` does not clear entries
already declared in `config.toml`, which were observed starting. Inherited
configuration is therefore handled the one way that works without moving a
credential: the selected home is inspected before launch and refused if it
declares MCP servers, hooks, plugins or other tool-bearing tables. Your login
stays exactly where it is.

Restricted sessions require process-group containment and a releasable advisory
lock, so they are available on macOS and Linux and fail closed elsewhere with
`restricted_session_unsupported_platform`. Ordinary sessions are unaffected on
every platform.

## Steering and capabilities

Every engine offers the same session methods. `Capabilities` describes support
as `native`, `composed`, `unsupported`, or `unknown`, with a reason. Unknown
means not yet established against the running CLI; it is not a positive claim
of support. Runtime protocol errors remain authoritative.

- Codex uses the local app-server protocol and native `turn/steer`.
- Claude uses `claude -p` with JSON stdin/stdout. Steering is composed from an
  acknowledged interrupt, the old turn's terminal result, and a new user turn
  in the same process. Session restart/resumption is separately supported.
- `RequireNative` rejects composed steering. Receipts identify the strategy and
  resulting turn. The expected turn ID prevents a stale request redirecting
  newer work.

User-message queueing remains application policy. Interruption never means a
tool's external effects were rolled back. Failed or uncertain control operations
must be reconciled before retrying.

## Account, quota and context inspection

Use the same API for either engine, optionally selecting a binary and login home:

```go
inspection, err := session.Inspect(ctx, session.Options{
    Engine: session.Codex, // or session.Claude
    // Binary: "/path/to/codex", Home: "/path/to/login-home",
})
// Inspect returns partial data alongside errors: the account may be known even
// when the installed CLI does not support quota inspection.
if inspection.Account.Known() {
    showPlan(inspection.Account.Plan) // empty means the plan was not reported
}
for _, window := range inspection.Quota.Windows {
    if !window.IsStale(time.Now(), 5*time.Minute) && window.UsedPercent != nil {
        showAllowance(window.ID, *window.UsedPercent, window.ResetsAt)
    }
}
```

`Inspect` performs no model inference and creates no conversation. It runs the
selected CLI in a temporary neutral directory, using its existing login, and
cleans up the child and directory. It never reads credential files, copies
credentials, or calls a provider's account endpoint itself. Session prompts,
working directories and execution policies in `Options` are ignored by this
inspection operation. Claude inspection uses `--safe-mode`.

A persistent `Session` also offers `ReadAccount(ctx)`, `ReadQuota(ctx)`,
`ReadContext(ctx)` and the nonblocking, no-I/O `Telemetry()` snapshot getter.
Turn events include `account`, `quota` and `context` updates when emitted by the
CLI; idle updates remain accessible through `Telemetry()`. A turn's `Result`
includes its latest streamed `Context`. Consume events promptly as usual.

| Data | Codex | Claude |
| --- | --- | --- |
| Account/plan | `account/read` and `account/updated` | `initialize.account`; reopen or inspect again to observe a changed login |
| Quota | `account/rateLimits/read` and update notifications, retaining separate limit buckets | Experimental `get_usage` with `skip_behaviors: true`; `rate_limit_event` while running |
| Context | `thread/tokenUsage/updated`: latest active size and effective window | Experimental `get_context_usage` with `detail: summary`; latest assistant input and matching model capacity from stream events |

Claude's summary uses local estimates and avoids per-category token-count API
calls; it is labelled `Estimated`. Streamed Claude occupancy is also an estimate:
latest input plus cache tokens, excluding pending output/tool results. Codex's
context uses `last.totalTokens`, **never** accumulated session totals. Reading
Codex context returns the latest streamed observation without freshening its
timestamp; a newly opened session may not have one yet. Compaction invalidates
old occupancy until another observation arrives. No method triggers compaction.

Telemetry distinguishes evidence from capability. `Capabilities` acknowledges
native support after a successful response/event. Unsupported methods return
`ErrUnsupported`; other failures do not prove unsupported capability. Claude's
experimental schemas may change; observed with CLI 2.1.272. Older versions can
still run sessions even when an inspection method is unavailable.

Empty `Quality` means unavailable, `Measured` means a native reported value, and
`Estimated` marks estimates. `ObservedAt` is when the library observed the CLI's
response, which may itself be cached. `IsStale(now, maxAge)` also checks explicit
invalidation. Refresh failures preserve prior data and timestamps as stale;
individual windows carry timestamps because stream events may update only some
windows. `Complete` distinguishes a snapshot from an incremental update, not a
promise that the provider exposed all its limits.

Optional fields are pointers: missing does not mean zero, free or unlimited.
Used percentages may exceed 100; only `RemainingPercent()` clamps its derived
remainder. Absolute caps appear only when reported with their unit (`Allowance`);
there is no invented tokens-per-subscription conversion. Named model windows may
overlap account-wide windows: they are constraints, not quantities to sum.

Applications decide when to poll, pause, select another engine, reserve headroom,
or compact. Account metadata can include personal information; keep it private.
These inspection methods do not bind a session reference to a cryptographically
verified identity and do not change authentication.

Protocol references: [Codex app-server](https://learn.chatgpt.com/docs/app-server),
[Claude programmatic CLI](https://code.claude.com/docs/en/headless). The library
speaks to `claude -p` directly; it does not depend on the Claude SDK.

## Native runs and transcripts

`native.Run` invokes either CLI with one `Config` and `Request` shape. Use
`native.NewStream` to receive typed events and render a common readable
transcript. Reuse a stream only for sequential resumes of the same session.
The application supplies its output schema and decides whether an incomplete
report warrants another turn; the library does not retry autonomously.

Native arguments and environment overrides are trusted configuration. They
must not originate in model output. Raw tool content and transcripts can contain
private material; the library does not make them safe for public display.

## Authentication and resource handling

Binary and home paths are optional per configuration. The library never changes
the parent process's environment or copies credentials. Constrained completion
and interactive sessions filter ambient provider overrides; native runs can
inherit the caller's environment explicitly for compatibility with CLI setups.
A separate working directory does not require a separate account. Selecting a
custom home can select a different login namespace; authenticate through the
CLI's supported flow for that home.

Unix subprocesses run outside the parent's terminal process group. Windows
subprocesses start suspended and are assigned to a job before running. Context
cancellation terminates contained descendants. Event/output bounds keep a noisy
CLI from growing memory indefinitely where the API advertises those bounds.

Usage preserves provider-specific accounting scopes. Missing or interrupted
usage can be unknown; zero counters do not necessarily mean a free run. A
rejected or abandoned constrained completion still returns any authoritative
terminal accounting its CLI reported, because a failed request can have been
billed. Reported costs are provider API-rate valuations, not a statement of
subscription charges.

## Development

```sh
go test -race ./...
go vet ./...
```

Automated tests use synthetic streams and fake CLI processes. They never invoke
paid models or touch real account credentials. CI runs on Linux, macOS, and
Windows. Consumers depend on published module tags, not sibling-directory
`replace` directives.

Licensed under [PolyForm Perimeter 1.0.0](LICENSE), matching the sibling
`lib-agent-*` libraries.

## Manual native compaction

`session.Compact(ctx)` requests compaction only while the session is idle and
returns a `*Turn`. Drain its events and await `Wait` before starting another
turn. Codex acknowledges `thread/compact/start` before doing the work; the turn
ID is empty until `turn/started`, and successful completion requires the native
terminal event. Compaction emits `compaction_started`/`compaction_completed`
item events and invalidates stale context measurements until a fresh observation.
Cancellation closes the session, as with `StartTurn`.

`Capabilities.Compact` starts unknown for Codex and becomes native on a successful
request, or unsupported on an explicit method rejection. Claude reports
unsupported: its interactive `/compact` command is not a verified stream-json
control operation. The library never substitutes a summarization prompt.
The protocol reference is the [Codex App Server manual compaction section](https://learn.chatgpt.com/docs/app-server).
