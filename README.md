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
usage can be unknown; zero counters do not necessarily mean a free run. Reported
costs are provider API-rate valuations, not a statement of subscription charges.

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
