# lib-agent-harness

One Go interface for running AI agents and models through any supported
harness: the Codex, Claude and Grok CLIs with their own logins, or any
OpenAI-compatible HTTP endpoint or gateway. An application written against it
can let its users choose the AI service behind each kind of invocation.
Choosing a service changes configuration, not code.

The library launches local CLI binaries with their native login, or calls an
explicitly configured API endpoint with a caller-supplied credential source.
It does not require a hosted execution service. The selected provider's account
and billing rules still apply.

```sh
go get github.com/shhac/lib-agent-harness
```

Requires Go 1.26.4 and, for a CLI engine, that CLI installed locally. macOS,
Linux and Windows are supported. CLI protocols evolve: pin and test your
deployed CLI versions, and handle unsupported capabilities at runtime.

## One vocabulary

The root package `harness` defines what every execution mode shares:

- `Engine`: `harness.Codex`, `harness.Claude`, `harness.Grok` or
  `harness.OpenAICompatible`. Its spelling is stable and safe to persist.
- `Provider`: where inference comes from. A CLI engine reads `Provider.CLI`
  (binary and login home); an API engine reads `Provider.API` (base URL,
  dialect, credential source). Setting the other half is refused.
- `Support(engine, operation, feature)`: the one question to ask instead of
  comparing engine names. It returns `native`, `composed`, `unsupported` or
  `unknown` with a reason, for the operations `Complete`, `Run`, `Session`,
  `Models` and `Account` and features such as `Effort`, `StructuredOutput`,
  `Resume`, `Steer`, `Compact`, `RestrictTools`, `CostReport` and `Quota`.
- `Usage` and `Cost`: token accounting in one shape. `Input` counts every prompt
  token, cached or not, and the cache figures are parts of it that are split out
  only when the provider reported them (`CacheKnown`). `Known` false means
  unavailable, never zero. `Cost` is the harness's own valuation at API rates,
  reported only when the harness states it.
- `ErrorFacts(err)`: fixed-vocabulary facts (engine, operation, family, cause,
  code, exit status, retry-after, and when an exhausted quota resets) for any
  error any mode returns. Error text
  never contains provider output or credentials.

```go
provider := harness.Provider{Engine: harness.Grok} // or Codex, Claude
if c := harness.Support(provider.Engine, harness.Session, harness.Available); !c.Usable() {
    return fmt.Errorf("sessions unavailable: %s", c.Reason)
}
```

Unsupported options are refused before any work starts, never silently
ignored, and no option ever widens what an agent may do.

## Choose the execution contract

| Package | Contract |
| --- | --- |
| `completion` | The model returns text and proposed application tool calls. Native tools are proven absent before inference; the application authorizes and executes proposals. |
| `native` | One native agent invocation, optionally resuming a session: an inline JSON-schema report, tool activity, a readable transcript, and usage. |
| `session` | Persistent bidirectional sessions with turns, streaming events, interruption, resumption, capability-aware steering, and restricted or sandboxed tool hosting. |
| `catalog` | The models and reasoning efforts an engine offers, without inference. |
| `account` | Login, plan, subscription quota windows and credits, without inference. |
| `process` | Shared subprocess-tree containment, including Windows suspended-start job assignment. |

These are explicit execution modes. A native agent session must not substitute
for constrained completion when the application relies on native tools being
unavailable. Applications own prompts, orchestration, scheduling, tool
permissions, budgets, durable state, and retry decisions.

## Constrained completion

```go
result, err := completion.Complete(ctx, completion.Config{
    Provider: harness.Provider{Engine: harness.Claude}, // or Codex, Grok
    Model:    "haiku", // discover with catalog.Discover; no library model default
    Effort:   "low",
}, []completion.Message{
    {Role: "system", Content: "Answer briefly."},
    {Role: "user", Content: "Suggest a short progress caption."},
}, nil)
// result.Message, result.Usage, result.Cost, result.ContextWindow
```

An OpenAI-compatible endpoint, such as a gateway, uses the same call and result.
Its bearer token comes from a per-request function rather than a configuration
string, so it is never retained by the harness:

```go
result, err := completion.Complete(ctx, completion.Config{
    Provider: harness.Provider{
        Engine: harness.OpenAICompatible,
        API: harness.API{
            BaseURL:         "https://gateway.example/v1",
            Dialect:         harness.OpenAIChatCompletions,
            Credentials:     tokenSource,
            EffortParameter: harness.EffortReasoningObject,
        },
    },
    Model:  "provider/model",
    Effort: "high",
}, messages, tools)
```

`Effort` is sent only when `API.EffortParameter` says where the endpoint reads
it: `harness.EffortReasoningEffort` (top-level `reasoning_effort`; OpenAI, xAI)
or `harness.EffortReasoningObject` (`reasoning.effort`; Vercel AI Gateway,
OpenRouter). A local model server that takes no credential sets
`API.Unauthenticated` instead of `Credentials`; that is refused for any
non-loopback URL.

For OpenRouter (`BaseURL: "https://openrouter.ai/api/v1"`), set
`API.OpenRouter` to a `*harness.OpenRouterRouting`. It is never inferred from
the URL. Its `RequireParameters` routes only to providers that honour every
parameter sent, tools included, and `DataCollection` (`"allow"` or `"deny"`;
anything else is refused as `api_openrouter_data_collection_invalid`) is sent
as the request's `provider.data_collection`; unset fields are not sent. The
option also says the endpoint follows OpenRouter's error shape, so an error
object inside a 200 response is classified by its integer `code` as that HTTP
status: an embedded 429 is `rate_limited` like a real one. A 429, including a
`:free` model's per-minute or per-day limit, is `rate_limited` and retryable,
with `RetryAfter` set only when the response sent a delta-seconds
`Retry-After` of at most an hour (never for an embedded one). The library does
not retry or back off; the caller does, and a daily limit may answer the retry
with another 429. A 402 from any endpoint is `insufficient_credits`
(`quota_exhausted`), never retryable. Routing is not part of a session's
`Ref`: resuming under different routing continues the same conversation.

Grok completion uses the operator's Grok login and requires `WorkDirRoot`.
Before the first credentialed launch per binary and configuration, the library
runs Grok against a local server that refuses inference and checks the request
it would send: no tools, only the caller's system text beside Grok's fixed
vendor context, and the expected response schema. Real runs use a private
runtime home that shares only Grok's login file, never the operator's MCP
servers, hooks, skills or plugins, and any reported tool aborts the run. Grok
also sends each conversation to its own title-generation model on the same
account. Grok completion is unavailable on Windows.

A failed request that may have been billed still returns its `Usage` and `Cost`
beside the error, with an empty message. See [completion](completion/README.md)
for the transport boundary, API restrictions, CLI compatibility probes, and
scratch storage.

## Skills

Tools an application relies on often come with skills: a directory holding a
`SKILL.md` and the files and scripts it references. Set `Skills` on
`completion.Config`, `native.Config` or `session.Options`:

```go
skills := harness.Skills{Provided: []harness.Skill{
    {Name: "release-notes", Dir: "/app/skills/release-notes", Scripts: true},
}}
```

- `harness.Support(engine, op, harness.ProvidedSkills)` says whether the harness
  loads them itself (`native`) or the library composes them (`composed`).
  `Skills.Delivery = harness.SkillDeliveryComposed` forces composition, for
  skills that should behave the same on every engine.
- **Native:** Claude and Grok load provided skills through a private
  `--plugin-dir`, and a sandboxed Codex session links them into its runtime
  home. Their scripts run with the agent's own tools, inside its sandbox where
  there is one.
- **Composed, in runs and ordinary sessions:** an index with each `SKILL.md`'s
  absolute path is added to the instructions, and the agent reads skills with
  its own tools. `Skill.Scripts` is enforced only where the library hosts the
  tools; here the agent runs scripts under its own permission policy.
- **Composed, in completion (every engine, including OpenAI-compatible
  gateways) and restricted sessions:** the library offers `load_skill` and, for
  skills marked `Scripts`, `run_skill_script`. In completion those calls come
  back in `Result.SkillCalls`, and `completion.AnswerSkillCalls` answers them.
  A script runs only when the caller passes its call there, with no shell, a
  contained process tree, a timeout, bounded output and a minimal environment,
  or through the caller's own `Exec` hook. Restricted sessions host the same
  tools beside the caller's and need `Options.SkillRun.WorkDir` for scripts.
- **Global skills:** `GlobalSkillsInclude` keeps installed skills where the
  mode already loads them. `GlobalSkillsExclude` is accepted only where the
  mode loads none (restricted sessions and sandboxed Claude). Anything that
  cannot be honoured is refused.

## Models

```go
models, err := catalog.Discover(ctx, provider)
for _, m := range models {
    // m.ID is what the engine accepts, possibly an alias such as Claude's "opus";
    // m.Resolved is the concrete model it selects today, where stated.
    // m.Name, m.IsDefault, m.ContextWindow (0 = not stated)
    if m.EffortsKnown {
        offer(m.ID, m.Efforts, m.DefaultEffort)
    }
    // Tool calling, where the endpoint lists the model's parameters.
    if supported, known := catalog.SupportsTools(m); known && !supported {
        hide(m.ID) // an API session would be refused
    }
}
```

Discovery performs no inference and invents no catalog: Codex and Claude read
their installed CLI's account-aware catalog, Grok its agent protocol's model
state, and an OpenAI-compatible endpoint its `GET /models`. Efforts are listed
only where the engine states them.

`m.Parameters` holds the request parameters an endpoint says the model accepts,
read from OpenRouter's `supported_parameters` (such as `tools`, `seed`).
`m.ParametersKnown` says the endpoint listed them, so an empty list means
"none". When it is false, `Parameters` is nil and means only that nothing was
said. CLI engines never list them. The list is taken only as an array of at most
128 non-empty strings of at most 64 bytes each, with duplicates dropped.
Anything else leaves that one model's parameters unknown, and the rest of the
catalog is still returned. `catalog.SupportsTools(m)` answers whether `tools`
is listed, and whether that is known at all.

## Native sessions

```go
s, err := session.Start(ctx, session.Options{
    Provider: harness.Provider{Engine: harness.Claude}, // or Codex, or Grok with Policy.GrokPermission
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

Grok sessions run `grok agent --no-leader stdio` over the Agent Client Protocol.
Creating one runs the operator's configured Grok hooks. Grok's agent mode asks
permission only where a permission rule or its mode says to, and otherwise runs
edits and shell commands itself, so a Grok session has no permission default:
set `Policy.GrokPermission` to `session.GrokDenyWhenAsked` or
`session.GrokAllowWhenAsked`, which answer the requests Grok does send. Neither
is a read-only mode. A model or effort Grok would substitute is refused before
the first prompt. Steering is composed (cancel, then prompt). Restricted,
sandboxed and compacted Grok sessions, and Grok quota, are unsupported.

Consume turn events while the turn runs. `Wait` does not drain the stream;
backpressure fails explicitly instead of silently dropping tool activity.
Cancellation of a wait only stops waiting. Interruption and session closure are
separate operations. Tool events say what an agent did (`harness.ToolActivity`):
`tool_started` carries the tool's arguments in `Input` (always valid JSON) and
`tool_completed` carries its result text in `Output`, with `Status` saying
whether it failed and `ExitCode` where a command reports one. Codex reports
command, cwd, output and exit code, file changes, MCP and dynamic tool
arguments and results, and web search queries; Claude its `tool_use` input and
`tool_result` content; Grok `rawInput` and `rawOutput` or content; an API
session the model's arguments and the handler's result. A hosted call is
reported once, by the engine's own events. Each of `Input` and `Output` is
bounded to `session.MaxToolPayloadBytes` (64 KiB) with `InputTruncated` or
`OutputTruncated` set; a truncated `Input` becomes a JSON string holding the
head of the original text. Payloads are passed through unredacted apart from
the library's own tool-channel credential: they can contain private data, and
applications apply their own visibility rules.

Images a tool returns, such as a browser screenshot or a generated image, are
taken out of the text and carried on `tool_completed` as `Images`, decoded
(`MediaType` and raw `Data []byte`):
at most `session.MaxToolImages` (4), each at most `session.MaxToolImageBytes`
(4 MiB), PNG, JPEG, GIF or WebP, with `ImagesOmitted` counting the rest.
Claude's image blocks are checked live (a claude-in-chrome screenshot), as are
Codex's native generated images (Codex 0.159.2). A Codex `imageGeneration`
completion carries the CLI's `savedPath` in `Output` and decodes its base64
`result` into `Images`; base64 never enters the text payload. The library does
not open the reported path. Go callers receive bytes directly; JSON-serializing
an event encodes its byte fields as base64. Oversized, malformed or unsupported
image payloads increment `ImagesOmitted`, while the path remains available under the normal
text bound. Failed generations retain their native status and carry no image.
Codex MCP results and Grok content are read as their protocols declare them, and
`harness.Support(e, Session, ToolImages)` says which.

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

## Sessions over an OpenAI-compatible endpoint

An endpoint is only a model, so the library is the agent. `session.Start`,
`Open` and `Resume` accept a `harness.OpenAICompatible` provider with the same
Session, Turn, Event and Result shapes as the CLIs:

```go
s, opened, err := session.Open(ctx, session.Options{
    Provider:    harness.Provider{Engine: harness.OpenAICompatible, API: api},
    Model:       "provider/model",
    RuntimeHome: "/private/state/gateway-sessions", // owner-only; holds transcripts
    Restriction: &session.Restriction{Tools: session.ToolHost{
        Server: "app", Tools: tools, Handler: handler, // no Bridge or Dir: called directly
    }},
}, storedRef)
```

- **Tools:** only your hosted tools and composed skills exist, because the
  library writes every request. Your handler runs one call at a time, with the
  same closing-tool and settlement rules as restricted CLI sessions.
- **Models without tool calling:** every session sends tools, so pass the
  catalog entry you chose the model from as `Options.CatalogModel`. When that
  entry lists its parameters without `tools`, `Start`, `Open` and `Resume`
  refuse with an `*UnsupportedError` whose code is
  `session.RefusedModelWithoutTools` (`model_without_tool_calling`), before
  any file is written or request sent. With no entry, or one whose
  `ParametersKnown` is false, the session goes ahead, because unknown is not
  "no": the library never runs discovery for a session, and a failed
  discovery never blocks a model you configured. The entry's `ID` (or its
  `Resolved`) must equal `Model`, otherwise the session is refused with
  `RefusedConflict`. The entry is advisory and not part of a `Ref`. A model
  that lists `tools` can still fail mid-run, as a typed turn failure. A CLI
  engine refuses `CatalogModel`.
- **Each turn:** the whole history is resent (Chat Completions keeps no server
  state) until the model answers without tool calls, bounded by
  `Options.Loop`, which also carries an optional per-response `MaxOutputTokens`
  cap.
- **State:** the conversation is an fsynced transcript under
  `RuntimeHome/sessions/<id>`, locked while open, and never holds a
  credential.
- **Crash recovery:** a call interrupted by a crash is answered as having an
  unknown outcome and is never run again (`Session.Recovered()` reports it).
- **Turn control:** interrupting cancels the request and the running handler.
  Steering is composed.
- **Not offered yet:** compaction, streamed text deltas, quota and account.

### Workbench, stage 1

Set `Options.Workbench = &session.Workbench{}` for read-only workspace tools
on an OpenAI-compatible session, on Linux, macOS or Windows. `WorkDir` must
be an absolute existing directory; it and `RuntimeHome` must not contain
one another. Its resolved path and workbench permissions form part of the
resume reference. CLI engines refuse this option.

- `read_file(path, offset, limit)` reads UTF-8 text: files at most 16 MiB,
  at most 2,000 lines and 64 KiB per result, with continuation notes.
- `list_files(path, depth)` lists at most 2,000 entries, to depth 8,
  visiting at most 20,000 names. Links, FIFOs, linked files and mounts are
  marked; Windows omits the listing's linked mark but still refuses linked
  reads. Links are never followed while listing.
- `search_files(pattern, literal, path, glob)` uses RE2, or literal text,
  with patterns at most 4,096 bytes. It returns at most 200 matches in
  `relative:line: text` form, cutting each result line to 400 bytes on rune
  boundaries.
  It skips files over 1 MiB, NUL in the first 8 KiB, invalid UTF-8, links,
  non-regular files, hard links and mounts, with counts. A glob containing
  a slash matches the workspace-relative path; otherwise it matches the
  basename.

Listings and searches skip `.git` and `.harness-workbench-*.tmp`, use bounded
walks, retain partial results with an incomplete note, and state truncation.
Neither observes `.gitignore`. Results also fit the host's `MaxResultBytes`,
which must be at least 4,096. All six workbench tool names (including planned
write and command tools) are reserved when the option is set.

Opened handles must stay on the root's mount; regular files must have one
link. Refusals include `file_linked`, `file_other_mount`, `file_not_regular`,
`file_too_large`, `file_not_text`, `file_reserved`, `file_outside_workspace`,
`file_path_invalid`, `file_not_found`, `file_not_directory`,
`file_is_symlink`, `path_through_symlink`, `path_not_listed`,
`file_unreadable` and `arguments_invalid`. When mount identity cannot be
established, launch refuses with `workbench_mount_check_unavailable` before
creating a transcript.

One workspace worker executes the tools. Cancellation waits for its current
I/O to settle. After a 10-second cancellation grace, a stuck read fails the
session with `workspace_io_stuck`; no later call or turn is admitted. The
transcript records an unknown outcome, shutdown releases its lock, and
`Release` returns the error. `Close` remains void. The single abandoned
worker and its read handle remain until the syscall returns. Health preserves
an earlier session failure; Release still reports abandoned workspace I/O.

`harness.Support` reports `WorkspaceRead` as `Composed`, `WorkspaceWrite`
as `Unsupported`, and `RestrictTools` includes the library's workbench tools.
A caller-supplied `CatalogModel` known to lack tools refuses before launch;
unknown tool support proceeds.

**Data caveat:** through gateways such as OpenRouter, prompts and every tool
result, including file contents, go to third-party model providers under their
own data policies; some free endpoints may log or train on inputs. Keep the
workspace free of secrets, outside hard links and mounts. File tools reject
multiply-linked files and other mounts. Keep other processes from injecting
outside data while the session runs.

Stage 2 remains planned: editing files and running commands under a sandbox
proved before launch. See the [design](design-docs/2026-09-29-api-workbench.md).

## Long-lived conversations: Open and caller context

`session.Open(ctx, options, ref)` is for a caller that keeps one conversation
across its own restarts. It resumes the reference's conversation when it can
and otherwise starts a new one, and `Opened` says which:

```go
s, opened, err := session.Open(ctx, options, storedRef) // storedRef may be nil
if err != nil { return err } // hold: ErrUnreclaimed, ErrLeaseHeld, capability, transport
switch {
case opened.Resumed:                                   // same conversation
case opened.Fresh == session.FreshIncompatible:        // another engine or configuration
case opened.Fresh == session.FreshUnavailable:         // the harness no longer has it
}
save(s.Ref())
```

- A restricted session's `Dir` is reclaimed first, under its assignment lease.
  A harness Open cannot confirm gone is returned as `ErrUnreclaimed`, and a lease
  another session holds as `ErrLeaseHeld`: Open never starts a harness over one
  that may still be running, and never reclaims one a live session is driving.
- **Claude**: the transcript must exist before `--resume` is tried. Claude Code
  keeps it at `<config dir>/projects/<key>/<id>.jsonl`, where the config
  directory is `Options.Home` and the key is the resolved working directory with
  every character that is not an ASCII letter or digit replaced by `-` (cut to
  200 and suffixed with a hash when longer), and its resume also accepts a single
  match in any other project folder. Open applies the same rule. A resumed Claude
  that exits during startup also counts as unavailable.
- **Codex**: a `thread/resume` the server refuses counts as unavailable.
- Anything else is returned with no session.

A restricted session's reference no longer covers its tools. The restriction is
re-proved on every launch and Claude's startup frame is cross-checked against the
tools configured for that launch, so a release that edits a tool's description,
schema or set resumes stored conversations under the new surface. The tool
server's name and the runtime home still have to match. (References stored for
restricted sessions by versions before v0.4.0 stop matching once.)

`Options.Context` lets the caller refresh what a long conversation knows about
its domain without the harness ever replaying history itself. It is asked at the
start of a turn whose conversation is new (`ContextStarted`) or was compacted
since the last turn (`ContextCompacted`: Claude's `compact_boundary`, auto or
manual, or Codex's `contextCompaction` item, including a `Compact` turn). The
text goes ahead of that turn's input:

```text
<caller-context reason="compacted">
…your text, verbatim…
</caller-context>

…the turn's input…
```

Empty text sends the input unchanged. A handler error fails `StartTurn` and
keeps the reason pending; it clears only once a turn carrying it has been
accepted, so each pending reason is delivered once. A compaction during a turn
is delivered with the next one. A restricted session keeps the pending reason in
`Dir/context.pending` (owner-only, beside the launch record), so a restart
between the compaction and the next turn does not lose it; an ordinary session
keeps it in memory.

## Restricted sessions with caller-hosted tools

`Options.Restriction` opts one session into a stricter contract: the harness's
own tools are removed, inherited customization is disabled, and your tools
become the session's entire surface. Sessions opened without it are unchanged.

```go
s, err := session.Start(ctx, session.Options{
    Provider: harness.Provider{Engine: harness.Claude},
    Model:    "haiku",
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

A restricted Codex session runs in `Options.RuntimeHome`: a durable private home whose
configuration this library writes, sharing only the login from `Options.Home`.
That is how a worker gets the operator's account without the servers, hooks,
plugins and trust settings that live beside it — none of which Codex's
`app-server` has any flag to ignore. The credential moves between two owner-only
directories in private application state and never reaches a workspace, a tool
result, a model, an error or a log; no API key is substituted for it. Which copy
is authoritative is decided by digest: a source that still matches what the home
was given has not changed, so a refresh the harness made wins; a source that has
changed is a new login and wins instead; a source that has been removed is a
logout and is left removed. A refresh reaches the source as each turn ends, and
a running session takes another's refresh before its next turn, because a Codex
refresh token is single use: two sessions refreshing copies of one login spend
it twice, and the second is refused (`refresh_token_reused`). A running session
is never moved to a different account's login, and a copy holding a refresh the
source did not take is left for the next launch to resolve. Writes to the
source are serialized by a `.agent-harness-login.lock` beside it; the
operator's own CLI does not take it. Restricted Claude sessions use the selected native
login and Claude Code's restricted mode; they do not use this credential-copy route.

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

`CancelTools` pauses the channel: it stops every call this session admitted —
executing *and* queued — and refuses further ones until `ResumeTools`, which a
new turn does for you. Cancelling only what happened to be running left the
queue behind it to execute afterwards, which is a write arriving after the work
was reported as stopped. A pause is not the same as the channel being finished:
a closing tool ends the work, a pause suspends it. `ToolsSettled` reports that
neither kind of call is outstanding.

A composed steer replaces the turn, and the replacement is bound to the
session's lifetime rather than to the bounded context that requested the steer.
Cancelling a control request must not end the work it started.

Tool calls execute one at a time. A native turn will ask for several at once,
and running them concurrently would make "after the work was reported" an
ambiguous claim — a write or a test could still be in flight when a closing tool
decides the assignment is finished, and cancelling it afterwards does not undo
it. A tool declared `Closing`, or a result with `Closes` set, therefore runs with
nothing else in flight and latches the channel only if it actually succeeded: a
finish whose arguments the handler rejected has not finished anything.
Everything queued behind it is refused without executing. `Session.ToolsClosed`
reports that state.

`ToolHost.MaxResultBytes` (default 64 KiB) bounds every answer to a call that
the model sees. That covers a result, a handler's error, a cancellation, a
refusal, and the library's answer for a call that never ran or whose outcome
is unknown. A longer answer is cut on a character boundary and ends with a
note that says how many bytes were left out. The note counts towards the
limit.

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
`--ignore-user-config` or `--ignore-rules` (those belong to `exec`), and
overriding the server table with `-c mcp_servers={}` does not clear entries
already declared in `config.toml`, which were observed starting. Inherited
configuration is therefore never read at all: the session runs in
`Options.RuntimeHome`, whose configuration this library writes, and only the
login is shared into it from `Options.Home`, as described above.

Restricted sessions require process-group containment and a releasable advisory
lock, so they are available on macOS and Linux and fail closed elsewhere with
`restricted_session_unsupported_platform`. Ordinary sessions are unaffected on
every platform.

## Sandboxed sessions with native tools

`Options.Sandbox` keeps a session's own tools and puts everything it executes
under the installed CLI's OS sandbox: no network, and project writes only
inside `WorkDir` when `Sandbox.Write` is set. Claude Code's shell may also write
its own private session temporary directory. Codex's shell may not write
`/tmp` or `$TMPDIR` at all.

```go
s, err := session.Start(ctx, session.Options{
    Provider:    harness.Provider{Engine: harness.Codex},
    WorkDir:     "/private/workspace",
    RuntimeHome: "/private/state/codex-runtime", // Codex only; shares the login
    Sandbox:     &session.Sandbox{Write: true},
})
```

The sandbox is proved before any credentialed launch, without inference, and
the result is cached per resolved binary and sandbox:

- **Codex:**
  - The session runs in a private `RuntimeHome` under a dedicated permission
    profile: filesystem read, the workspace writable (or read-only), `.git`
    read-only, `.git`, `.codex` and `.agents` read-only, network disabled,
    approval `never`, web search off unless `Web` is set, and the connector,
    plugin, hook, browser, computer-use, multi-agent and dependency-install
    features disabled.
  - A canary runs under that profile through `codex sandbox`. Writing outside
    the workspace, to `/tmp` or to the inherited `$TMPDIR` must fail, as must
    reaching a loopback listener owned by the probe, or writing the
    workspace's `.git`. The canary must also report that it ran, so an empty
    result is never read as success.
  - Once the harness starts, thread/start and thread/resume must report that
    profile, with network closed and temporary directories excluded, before
    the first prompt. Otherwise the session is closed.
  - The legacy `sandbox` thread mode is never sent, because it silently
    replaces the profile.
- **Claude Code:**
  - The session loads only the library's settings (`--setting-sources=`,
    `--strict-mcp-config`, hooks disabled): sandbox enabled and
    fail-if-unavailable, no unsandboxed retries, an empty network allowlist,
    `dontAsk` permissions, WebFetch, WebSearch and MCP tools removed (see
    `Web` and `Tools` below), and
    `Edit` allowed only inside `WorkDir` (resolved through symlinks). The
    network allowlist is strict.
  - A writing session cannot change `WorkDir/.git`, as with Codex.
  - A read-only session also loses Edit and Write, and denies sandboxed writes
    to `WorkDir`.
  - Its shell cannot read the home directory outside `WorkDir`
    (`blockReadsOutsideWorkingDirectories`). `Sandbox.Read` reopens named
    directories read-only, such as a Go module cache a build needs. A read
    path must be absolute, and none may contain the home directory. Codex
    already reads everything, so `Read` changes nothing there.
  - No instruction files load, not even the workspace's own: with every
    settings source dropped, Claude Code 2.1.280 loads none, and the
    operator's user and ancestor files are also excluded by name as a second
    guard. Point the session at a repository's `AGENTS.md` or `CLAUDE.md` in
    its prompt if it should follow them. Skills and auto-memory are off.
  - `claude sandbox status` with the same settings, asked from a throwaway
    home, must report the sandbox supported, enabled and strict, with no
    unavailable reason. This is the CLI's own report, not a canary: Claude
    offers no way to run a sandboxed command without inference.

A sandboxed session inherits only an allowlisted environment from its
caller (`PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `LC_*`,
`TZ`, `TMPDIR`), never whatever else the caller happened to export.
`Options.Env` adds ordinary `KEY=VALUE` settings to any session, for example
a build cache or `TMPDIR` inside the workspace so a sandboxed build can write
them. Keys the harness manages, and keys that would change the CLI itself
outside its sandbox (loader and Node options, proxies and certificates,
`GIT_*`, homes, provider credentials), are refused.

### Web access

`Sandbox.Web` lets a sandboxed session search and read the web. Nothing else
about the sandbox changes, and the shell's network stays closed:

- **Claude Code** gains WebSearch and WebFetch, allowed by bare permission
  rules. A `WebFetch(domain:...)` rule is never written, because Claude Code
  adds every domain such a rule names to the shell's network allowlist too.
  WebFetch runs in the CLI process, outside the OS sandbox.
- **Codex** runs with `web_search="live"`. Checked against codex 0.156.1 with
  a provider that declares web search support, this offers the model a `web`
  tool that searches and opens pages through the provider, not from the
  shell. Whether a real login's provider offers it has not been checked
  without inference.

Either way it is an outward channel for anything the session can read: a
fetched URL can carry data out. A reference records whether a sandbox had
`Web`, so a resume cannot change it; a sandbox without it keeps its earlier
reference digest.

### Loopback networking

`Sandbox.Loopback` lets a sandboxed Claude session start a local server and
request it: its shell may bind and connect to this machine's own addresses and
nothing else. Claude Code's `allowLocalBinding` admits the machine's interface
addresses as well as loopback, so a server bound to one of those is reachable
too; no other host is, and the domain allowlist stays empty. Anything the
project needs at run time must therefore be local.

The claim is proved before each launch, without inference. The session's own
arguments drive Claude Code against a local provider that answers the first
turn with one scripted shell call, the canary, and reads its result back. The
canary must reach the probe's loopback listener, bind and reach one of its
own, and be refused `api.anthropic.com` on 443, which the probe has just
reached itself; if that address cannot be reached from outside the sandbox,
nothing is proved and the session is refused. Codex is refused: its sandbox
network is all or nothing.

A process the agent starts in the background, such as that server, is
stopped when the session closes (see Process containment below).

### The harness's own browser

`Options.Browser` (and `native.Config.Browser` for a run) switches on the
browser integration the harness ships, for that invocation only. The library
requests it only when set; leaving it unset preserves the CLI's own defaults
and configuration. It is never implied by another option:

- Claude Code: `--chrome` (Claude in Chrome), with the
  [Claude extension](https://chromewebstore.google.com/detail/claude/fcoeoabgfenejglbffodgkkbkcdhcgfn).
- Codex: `features.browser_use=true` and `features.browser_use_external=true`,
  with a configured native browser bridge in the selected `Provider.CLI.Home`
  and the [ChatGPT extension](https://chromewebstore.google.com/detail/chatgpt/hehggadaopoacecdllhhajmbjkdcmajg).
  The extension alone does not configure that bridge. The working local setup
  uses the ChatGPT application's native Node REPL MCP bridge (`node_repl`),
  browser plugins and native host. Restart the CLI after installing the extension.
  The library does not install plugins, copy another home's configuration,
  enable desktop control, or change site permissions.

Codex 0.159.2's app-server and exec flags and deferred Node REPL tools were
checked with disposable homes, dummy credentials and a local provider that
refuses inference. Chrome control was separately checked in a configured CLI
session by filling and validating a disposable local form. This establishes
browser control; it does not establish a video recording API or hidden-window
capture. Grok 1.0.41, API sessions and restricted sessions are refused.
Sandboxed Codex sessions are also refused: their isolated home cannot inherit
the owner's browser bridge safely. Use an ordinary native Codex session, or
leave `Browser` unset. `harness.Support(e, Session, SandboxedBrowser)` says
which engines admit the browser in a sandboxed session, for a caller that
sandboxes every session and must decide before offering the browser.

Before any prompt, Codex sessions check that `node_repl` advertises its `js`
and `js_reset` tools, including deferred tools. Claude sessions check that
`claude-in-chrome` is connected and advertises browser tools. Missing tools
produce a typed `session.CapabilityError` with code `browser_tools_missing`,
the extension URL and setup guidance. Codex's inventory proves bridge tools
are loaded, not that Chrome is connected. Native runs have no equivalent
startup check and retain the CLI's own diagnostics. Neither check executes a
browser action.

That browser is not sandboxed. It runs outside the OS sandbox, with that
Chrome profile's logins, and reaches whatever the extension's site
permissions allow. For agents:

- Give them a dedicated Chrome profile with the extension, signed in to
  nothing they should not use, rather than your everyday profile.
- Limit the extension's site permissions to the sites the agent should
  visit, typically the local dev server.
- For Claude, pair it with `Sandbox.Loopback`, so the agent's shell can start the project
  and the browser can use it, while the shell reaches nothing off the machine.

A sandboxed Claude session admits the browser's tools beside its own, with explicit
allow rules for the ones Claude Code 2.1.283 lists. `file_upload` (which reads
local files from outside the sandbox) and the shortcut tools (which start
another agent in the browser's side panel) are denied, and a tool a later
build adds stays refused until it is reviewed. `--strict-mcp-config` keeps
only the browser's server, claude.ai connectors are switched off, and the
startup report must show that server connected and no other server's tools.
Checked live: a sandboxed session served a page on loopback, read it through
Chrome, and found no `file_upload` tool.

### Background priority

`Options.Background` (and `native.Config.Background`) runs the harness, and
everything it starts, at background priority, so agent work yields to whatever
the person at the machine is doing. Once the harness starts, its process group
is niced to 10, which descendants inherit, including a server an agent
backgrounds into a process group of its own. If the priority cannot be
lowered, the launch is stopped rather than run at normal priority. It is not
offered on Windows, and it is not part of a `Ref`.

macOS's background band (`PRIO_DARWIN_BG`) is deliberately not used. v0.13.0
used it, and on a busy machine it made an agent's 5-second test suite take two
minutes and time out: it also throttles disk I/O and confines work to the
efficiency cores. Nice 10 alone ran the same suite in 5.5 seconds at the same
load while still yielding to foreground work.

### Caller-hosted tools beside native ones

`Sandbox.Tools` takes the same `ToolHost` a restriction does, and serves it the
same way: through the caller's bridge command (`session.RunBridge`), under the
assignment lease, with a launch record, `Reclaim`, `Open`'s reclaim-first
behaviour and `Release`. The session keeps its sandboxed native tools and gains
the hosted ones.

```go
s, err := session.Start(ctx, session.Options{
    Provider: harness.Provider{Engine: harness.Claude},
    WorkDir:  "/private/workspace",
    Sandbox:  &session.Sandbox{Write: true, Web: true, Tools: &session.ToolHost{
        Server:  "crew",
        Tools:   tools,
        Handler: handler,
        Dir:     "/private/state/crew-worker-1", // owner-only, outside WorkDir
        Bridge:  session.Bridge{Path: executable, Args: []string{"tool-bridge"}},
    }},
})
```

- The sandbox is proved first; the tool channel opens only after that, and the
  sandbox evidence stays cached across channels.
- **Claude Code** loads only this server (`--strict-mcp-config`, claude.ai
  connectors disabled) and allows exactly its tools. The wholesale `mcp__*`
  deny is dropped, because a deny outranks every allow and would refuse the
  hosted tools too; any other MCP tool has no allow rule, which `dontAsk`
  refuses. The startup report must show the server connected, every hosted
  tool present and no other server's tools, or the session is closed.
- **Codex** registers the bridge as an approved MCP server in its private
  runtime home, which declares no other server. The app-server launches the
  bridge outside the shell's sandbox; checked against codex 0.156.1, the
  sandboxed shell itself cannot connect to the channel's socket.
- `Dir` must lie outside `WorkDir`. A Codex session can still read it, as it
  can read the account's other files, but the channel refuses anything
  without its credential and the shell cannot reach the socket.
- Hosted tools run whatever their handler does, outside the sandbox.
- There is no pre-launch surface probe, as native tools are present by design.
  Codex's hosted tools are not checked at startup beyond the channel serving
  them.
- A reference includes the tool server's name, not the tools or the channel.

`session.VerifySandbox(ctx, options)` runs the same check without opening a
session. Failures are `*CapabilityError` values with `sandbox_unavailable` or
`sandbox_not_enforced`. The phase says whether anything was launched.

What this does **not** do:
- Reads are not contained. A session can read what the operating account can
  read.
- The model provider connection remains an outward channel.
- Claude Code's file tools are confined by permission rules; its OS sandbox
  covers only the shell.
- A sandboxed session without `Tools` has no bridge, so it has no launch
  record or `Reclaim`. A caller that restarts mid-turn should treat the turn
  as interrupted.
- The mode is unavailable on Windows (`sandbox_unavailable`).
- `Restriction` and `Sandbox` are mutually exclusive, and a sandbox refuses
  any policy it would otherwise have to override.

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

## Account, quota, credits and context

Usage is paid for in three ways, and an engine may use any of them:
subscription quota windows (percentages of a plan's allowance over a period),
token spend (the harness's own valuation at API rates, on `Usage`/`Cost`), and
credits (a prepaid or overage balance that can cover either). `account.Inspect`
reports quota and credits for every CLI engine in one shape, each part saying
whether it is known, and leaves combining them to the application:

```go
report, err := account.Inspect(ctx, harness.Provider{Engine: harness.Codex}) // or Claude, Grok
// Partial data can arrive beside an error: the login may be known when quota is not.
if report.Account.Known() {
    showPlan(report.Account.Plan) // empty means the plan was not reported
}
for _, window := range report.Quota.Windows {
    // Kind is session, weekly, weekly_model (with Model), monthly or other.
    if !window.IsStale(time.Now(), 5*time.Minute) && window.UsedPercent != nil {
        showAllowance(window.Kind, window.Model, *window.UsedPercent, window.ResetsAt)
    }
}
if report.Credits.Known() && report.Credits.Balance != nil {
    showCredits(report.Credits.Balance.Value, report.Credits.Balance.Unit) // exact decimal, currency or "credits"
}
```

| Engine | Login and plan | Quota windows | Credits |
| --- | --- | --- | --- |
| Codex | `account/read` | `account/rateLimits/read` | credit balance, spend control, rate-limit resets |
| Claude | `initialize.account` | `get_usage` and `rate_limit_event` | spend and extra-usage allowance, in minor currency units |
| Grok | agent protocol `check_subscription` | unsupported | unsupported |

A missing CLI is a classified `not_installed` failure. `session.Inspect` remains
for Codex and Claude and also returns a session's `Capabilities`.

Inspection performs no model inference and creates no conversation. It runs the
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

`native.Run` invokes Codex, Claude or Grok with one `Config` and `Request`
shape. Use `native.NewStream` to receive typed events and render a common
readable transcript. Reuse a stream only for sequential resumes of the same
session. The application decides whether an incomplete report warrants another
turn; the library does not retry autonomously.

```go
result, err := native.Run(ctx, native.Config{
    Provider: harness.Provider{Engine: harness.Grok}, // or Codex, Claude
    Model:    model,
    Grok:     native.GrokOptions{Telemetry: native.GrokTelemetryReduced},
}, native.Request{Prompt: "Review this change.", WorkDir: workspace, Schema: reportSchema}, nil)
if err != nil {
    facts, _ := harness.ErrorFacts(err) // fixed codes; the provider's own text is result.Failure
}
// result.Report is the schema-shaped report; result.Usage, result.Cost
```

- One inline `Request.Schema` works for every engine. For Codex, the library
  writes the schema and reads the report in a private directory outside the
  workspace, where a workspace-write agent cannot forge it. Codex constrains
  every message to the schema; `harness.Support(e, harness.Run,
  harness.ProgressMessages)` says whether an engine lets the agent write
  unconstrained progress messages first.
- Engine-specific execution settings live in `Config.Codex`, `Config.Claude`
  and `Config.Grok`. Options for another engine, and `Args` that repeat a flag
  the library manages, are refused.
- Failures are `*native.RunError` values with harness facts. The provider's own
  account of a failed turn is only in `Result.Failure`.
- A Grok turn that ends with any stop reason other than `end_turn` is a failure.
  Grok's `Tools` removes built-in tools only: MCP helper tools remain, as do
  MCP servers imported from other harnesses unless `GrokTelemetryReduced` is set.

Grok's zero-value telemetry policy preserves the installed CLI's normal
behaviour. `native.GrokTelemetryReduced` turns off Grok's documented
client-telemetry, trace-upload, feedback, auto-update and memory controls, and
its imports of Claude, Cursor and Codex skills, rules, agents, MCP servers,
hooks and sessions, for that invocation. It is not a no-egress guarantee:
inference and any enabled tool or provider traffic still leave the machine. It
does not change the xAI account's coding-data sharing or retention settings
(`/privacy`, Zero Data Retention), and leaves external OpenTelemetry to the
operator's own collector (`GROK_EXTERNAL_OTEL`) as configured.

Native arguments and environment overrides are trusted configuration. They
must not originate in model output. Raw tool content and transcripts can contain
private material; the library does not make them safe for public display.

## Authentication and resource handling

Binary and home paths are optional per configuration. The library never changes
the parent process's environment. Restricted Codex sessions share login material
into a private runtime home as described above; other invocation modes use the
selected native login directly. Constrained completion and interactive sessions
filter ambient provider overrides; native runs can inherit the caller's
environment explicitly for compatibility with CLI setups.
A separate working directory does not require a separate account. Selecting a
custom home can select a different login namespace; authenticate through the
CLI's supported flow for that home.

### A locked keychain

Claude Code keeps its login in the macOS login keychain, and every Claude
process reads it at startup. While the keychain is locked, each start raises
its own unlock prompt, and several at once have wedged SecurityAgent. So
before any operation launches Claude on macOS (sessions and their checks,
native runs, completion, discovery, account inspection), the library asks
the keychain's own status, which never prompts, through
[lib-agent-keyring](https://github.com/shhac/lib-agent-keyring). If it is
locked, the operation fails with `harness.CodeKeychainUnavailable`
(`keychain_unavailable`, preflight family, not retryable) and starts nothing.
`harness.LoginStoreLocked(engine)` asks the same question, in about a
millisecond, so an application can stop polling until it is false again and
show one "unlock your keychain" message rather than a failure per poll.

### Process containment

Unix subprocesses run outside the parent's terminal process group. Windows
subprocesses start suspended and are assigned to a job before running. Context
cancellation terminates contained descendants. Event/output bounds keep a noisy
CLI from growing memory indefinitely where the API advertises those bounds.

An agent's background commands leave that group: Claude Code starts each in a
process group of its own, and once its shell exits it belongs to launchd. So
every launch carries a random token in `AGENT_HARNESS_LAUNCH`, which its
descendants inherit, and stopping or closing the launch kills every process of
this user that carries it, and those processes' descendants. Close does this
even after the CLI itself exited, so a native run's leftovers go with it, and a
session's go when it closes. It is a best-effort sweep, not a boundary: a
process that clears its environment escapes, and on macOS, which hides the
environment of its own platform binaries, so does one of those whose marked
parent has already exited. A shared helper that a harness starts on first use
would be stopped too, and started again by the next launch.

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

### Running the tests inside a sandbox

The suite also runs inside a sandbox, such as the one an agent runs its
commands in, provided it allows loopback connections: the provider stand-ins
and capability checks listen on `127.0.0.1`. Tests that need something else
such a sandbox commonly refuses first probe for it, once per test binary, and
skip with the probe's refusal when the environment denies it with a permission
error:

| Needs | Tests |
| --- | --- |
| Creating FIFOs, sockets, hard links and Windows junctions; a writable package directory | workbench containment tests; `TestWorkbenchFIFOAndSocketRefusal` binds directly in a workspace under `./.wbs-*` to keep the socket path short; Linux bind mounts require the CI mount setup |
| A Unix domain socket under `TMPDIR` | every test that opens a restricted or sandboxed session's tool channel (`session/`) |
| A process group of its own (setpgid) | the tests of what containment does with the group — detach, group cancellation, descendant pipes, escapees, sweeps (`process/`); the stand-in bridge and `Reclaim` tests (`session/`) |
| Reading process status with `ps` | the cancelled-group and sweep tests (`process/`), which otherwise could not tell a live process from a gone one |
| Lowering a process group's priority | `TestBackgroundLowersTheWholeTree` (`process/`) |
| Writing `/tmp` and `TMPDIR` directly | `TestOpenCanaryEscapesAreEachDetected` (`session/`): an outer sandbox contains the unsandboxed canary too, which then rightly reports no escape |

Any other probe failure fails the test, so a real fault is never hidden behind a
skip. `go test -v ./...` lists each skip with its reason, for example
`environment refuses a Unix domain socket under TMPDIR: listen: … operation not
permitted`. Harness launches in general are not gated. They always start in a
process group of their own, and that has to work wherever the library runs.

Setting `AGENT_HARNESS_TEST_NO_SKIP=1` turns every such skip into a failure. CI
sets it, so an unsandboxed run can never pass by skipping. The helpers live in
`internal/testenv`.

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

In a restricted Claude session it stays unsupported for a second reason:
`--disable-slash-commands` removes `/compact` along with every other command,
and lifting that would need its own review of the whole command surface. Claude
still compacts automatically. A caller that wants a manual compaction rebuilds
instead: summarize the conversation itself, `Open` a fresh session (no
reference), and return the summary from `Options.Context` for `ContextStarted`.
The protocol reference is the [Codex App Server manual compaction section](https://learn.chatgpt.com/docs/app-server).
