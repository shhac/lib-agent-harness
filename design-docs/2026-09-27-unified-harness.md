# One interface for every harness

Proposed 2026-09-27. Revised after review: usage keeps the full prompt as its
primary figure, error facts get new field names, the session `Ref` digest is
pinned by golden tests before anything moves, and increment 1 lands in ordered
sub-steps.

## Why

Applications built on this library should let their users choose the AI service
behind every kind of invocation, whether a Codex, Claude or Grok CLI or an
OpenAI-compatible gateway, without the application learning each one. Today the
choice leaks everywhere:

- Each execution mode has its own engine type: strings in `completion` and
  `native` (only `"openai-compatible"` is a constant, and `"grok"` exists only
  in `native`), and a typed value in `session`.
- Locating a harness is spelled three ways: `CodexBin`/`ClaudeHome` in
  `completion`, and `Binary`/`Home` in the other two. An empty home means a
  different thing in each package.
- Usage has three incompatible definitions of "input":
  - `completion.Usage.InputTokens` includes cache.
  - `native.TokenUsage.Input` excludes cache reads.
  - `session.Usage` is a third shape.
  Cost exists only in `native`.
- Errors come in two vocabularies: `completion.RequestError` and
  `session.ErrorFacts`. `native` returns untyped errors that carry provider
  text, against the `AGENTS.md` rule.
- Some config fields are silently ignored for some engines:
  - `native` Codex ignores `AllowedTools`, `PermissionMode`, `MaxBudgetUSD` and
    an inline `Schema` (it parses JSON it never constrained).
  - `session` Codex ignores `Policy.ClaudeTools`, even `[]`, which a caller
    reads as "no tools".
- Only `session` can say what an engine supports, and only for its own mode.

Both consumers compensate with the same per-engine code:

- **crew-assistant**
  - Engine switches in `EngineConfig`, `engine.New` and `CLIRef`.
  - Its own OpenAI HTTP transport and error mapping.
  - Hardcoded "can this engine compact / hold a chat session" checks.
  - Its own model and effort validation.
  - Quota windows mapped per engine.
- **crew-code-review**
  - Per-engine constructors and defaults.
  - Schema-file vs inline-schema delivery.
  - Its own Codex model catalog and login probes.
  - Quota window names mapped per engine.

This document fixes the shared vocabulary once, then moves each piece of
per-engine knowledge into the library.

## Principles

- **One vocabulary, three contracts.** `completion` (the model proposes, the
  caller executes), `native` (one agent run) and `session` (a persistent
  agent) stay distinct execution modes. What they share (engine, provider,
  usage, cost, capabilities, error facts) is defined once, in the root package.
- **Every field is consumed or refused.** A field the selected engine cannot
  honour is refused before launch with a typed capability error. Nothing is
  ignored, and permissions are never widened.
- **Ask, don't switch.** A caller asks `harness.Support(engine, mode, feature)`
  instead of comparing engine names. The static table is the library's claim;
  runtime evidence can promote an entry from unknown, never past what the table
  allows.
- **Persisted values stay stable.** Engine strings keep their spellings. Session
  `Ref` digests must not change for an existing configuration.
- **The `AGENTS.md` invariants are unchanged.** Restriction proofs, tool-free
  probes, "unknown is not free", no provider text in errors, and no retries.

## The root package

Module root, package `harness` (import `github.com/shhac/lib-agent-harness`). It
is a leaf: it imports no mode package.

```go
type Engine string

const (
    Codex            Engine = "codex"
    Claude           Engine = "claude"
    Grok             Engine = "grok"
    OpenAICompatible Engine = "openai-compatible"
)

func (e Engine) Transport() Transport // CLITransport or APITransport; "" if unknown

// Provider says where inference comes from. The engine's transport decides
// which half applies; setting the other half is refused.
type Provider struct {
    Engine Engine
    CLI    CLI
    API    API
}
type CLI struct {
    Binary string // "" = the engine's command on PATH
    Home   string // "" = each mode's existing default, unchanged
}
type API struct {
    BaseURL         string
    Dialect         Dialect          // required; OpenAIChatCompletions today
    Credentials     CredentialSource // func(ctx) (token, error)
    Unauthenticated bool             // loopback only
    EffortParameter EffortParameter  // required when Effort is set
}
func (p Provider) Validate() error
```

`OpenAICompatible` names the harness, not the model family: Grok models through
a gateway are `OpenAICompatible` with a model such as `xai/grok-4`. The `API` half
is in the root because remote sessions (increment 6) will use it too. Until
then, `native` and `session` refuse an API provider through `Support`.

Home defaults are not unified. `session` hashes the resolved `Binary` and
`Home` into `Ref` (session/options.go:58-89, 171-176), so any change to how an
empty home resolves would orphan stored sessions. Literal golden digests for
defaulted configurations land before `Provider` does.

`Model` and `Effort` stay plain fields on each mode's config. `Env`,
`RuntimeHome` and `WorkDir` stay mode-specific because their meaning differs
between modes (the environment is filtered in `completion` and `session`, but
passed whole in `native`).

### Usage and cost

```go
type Usage struct {
    Known      bool
    Input      int64 // every prompt token, cached or not (the OpenAI convention)
    Output     int64 // includes Reasoning
    CacheRead  int64 // part of Input
    CacheWrite int64 // part of Input
    Reasoning  int64 // part of Output
    // CacheKnown says the provider split its cache tokens out. Many gateways
    // omit that split; zero cache figures then mean unreported, not uncached.
    CacheKnown bool
}
func (u Usage) Fresh() (int64, bool) // Input - CacheRead - CacheWrite, when CacheKnown
func (u Usage) Total() int64         // Input + Output

type Cost struct {
    USD   float64
    Known bool // false: partial or unreported, never zero
}
```

Fresh input is a derived figure, because it cannot always be derived. The
alternative, a fresh `Input` field, would sometimes hold a guess that looks
measured. The context window is not usage: `session` sums usage across
responses, and a window must not be summed. It stays in each mode's own
snapshot or result.

- `completion.Complete` returns a `completion.Result{Message, Usage, Cost,
  ContextWindow}`.
  - Codex's `cached_input_tokens`, which is read but ignored today, sets
    `CacheRead`.
  - Claude's `total_cost_usd`, which is dropped today, sets `Cost`.
  - An OpenAI response's optional `prompt_tokens_details.cached_tokens` sets
    `CacheRead` and `CacheKnown`.
- `native.Result` replaces `TokenUsage`/`UsageKnown`/`CostUSD`/`CostKnown` with
  `Usage` and `Cost`. A missing cache field is no longer read as zero.
- `session.Usage` embeds `harness.Usage` and keeps its `Final` flag.

### Capabilities

```go
type Availability string // Native, Composed, Unsupported, Unknown (moved from session)
type Capability struct { Availability Availability; Reason string }

type Mode string    // ModeCompletion, ModeRun, ModeSession
type Feature string // FeatureMode (the mode itself), FeatureStructuredOutput,
                    // FeatureResume, FeatureInterrupt, FeatureSteer, FeatureCompact,
                    // FeatureEffort, FeatureModelDiscovery, FeatureCost,
                    // FeatureAccount, FeatureQuota, FeatureContext,
                    // FeatureRestrictTools, FeatureSandbox, FeatureHostedTools, …

func Support(e Engine, m Mode, f Feature) Capability
```

One static table, with tests, replaces `session.CapabilitiesFor` and the
consumers' engine checks. `session.Capabilities` remains the per-session runtime
record and starts from the table.

### Error facts

`session`'s `Facts`/`ErrorFacts` pattern moves to the root, widened to cover
completion's causes:

```go
type Facts struct {
    Engine     Engine        `json:"engine,omitempty"`
    Mode       Mode          `json:"mode,omitempty"`
    Family     Family        `json:"family,omitempty"` // capability, preflight, process, request, turn
    Cause      Cause         `json:"cause,omitempty"`  // rate_limited, authentication, context_limit, …, unknown
    Phase      string        `json:"phase,omitempty"`
    Code       string        `json:"code,omitempty"`
    ExitCode   *int          `json:"exit_code,omitempty"`
    RetryAfter time.Duration `json:"retry_after,omitempty"`
    Retryable  bool          `json:"retryable"`
}
func ErrorFacts(err error) (Facts, bool)
```

The family is not named `Kind`. crew-assistant persists `session.Facts`, whose
`kind` tag holds the family today, and reusing the name for the cause would
silently change what stored diagnostics mean.

`completion.RequestError`, `session`'s `CapabilityError`, `ProcessError`,
`TurnError` and `UnsupportedError`, and a new typed `native` run error all
implement it. The untyped `errors.New` refusals in `session` options become
capability errors. A caller
classifies any library failure the same way, whichever mode produced it.

## Status

- **Increments 1–4:** shipped in v0.6.0. Discovery lives in its own `catalog`
  package and account inspection in `account`. Features are keyed by
  operation (`Complete`, `Run`, `Session`, `Models`, `Account`), not by mode.
- **Increment 5:** shipped in v0.7.0 for Grok constrained completion (proven
  tool-free, with a private runtime home sharing only the login) and ordinary
  Grok sessions over ACP. Restricted and sandboxed Grok sessions remain
  unsupported.
- **Increment 7:** shipped in v0.7.0, with `MaxOutputTokens` for completion.
- **Increment 6:** shipped in v0.8.0.
- **Not yet:** streamed text deltas for API endpoints, composed compaction, and
  the Responses dialect.

## Increments

Each increment ships with its consumers updated, and each leaves the module
releasable.

1. **Shared vocabulary**, landed in this order so every commit builds:
   1. Golden fixtures: literal `Ref` digests for defaulted configurations, and
      usage fixtures per engine and mode.
   2. The root package: `Engine`, `Usage`, `Cost`, `Facts`, with mode types
      aliased to it (`session.Engine = harness.Engine`).
   3. `ErrorFacts` implemented by every typed error in every mode.
   4. Each mode switches to `harness.Usage`/`Cost`, fixing Codex completion's
      cache split.
   5. `Provider` replaces `Engine`/`Binary`/`Home` and completion's
      `CodexBin`/`ClaudeHome`/`API`, one package at a time.
   6. `Support`, replacing `session.CapabilitiesFor`.
   7. Delete the aliases, then update crew-assistant and crew-code-review.
2. **Native runs are engine-neutral.**
   - One inline `Request.Schema` for every engine. The library writes and reads
     Codex's schema and output files itself, in a private directory outside
     `WorkDir` (a workspace-write agent could otherwise forge the report), and
     clears the output before every resume. `SchemaPath` and `OutputPath` go.
     Codex constrains every message to the schema, not just the last; `Support`
     exposes that, since crew-code-review's WORKING loop depends on it.
   - `Args` refuses flags the library manages (schema, output, sandbox,
     permission and tool flags), so "consumed or refused" holds.
   - Typed run errors, with provider text only in `Result.Failure`, which
     callers read instead of the error string.
   - Engine-specific execution options move into `Codex`/`Claude`/`Grok` option
     structs, and one for a different engine is refused.
   - Codex's silently ignored fields are refused.
   - crew-code-review loses its schema-file plumbing.
3. **Model discovery for every engine.**
   - One `DiscoverModels(ctx, Provider)` covering Codex, Claude, Grok
     (`grok models`) and OpenAI-compatible (`GET /models`), returning models
     with their efforts and defaults.
   - The consumers' catalogs, hardcoded model lists and effort checks go.
4. **Account, login and quota for every CLI.**
   - Inspection gets normalized quota window kinds (five-hour, weekly, and
     per-model pools) and a classified "not installed" error.
   - Grok is added where its CLI exposes this.
5. **Grok everywhere.** Constrained completion, with a restriction proof, and a
   session adapter over Grok's agent mode. Each lands only once it is verified
   against the installed CLI, as `AGENTS.md` requires.
6. **Sessions for OpenAI-compatible endpoints, composed by the library.** An
   endpoint is only a model, so the library is the agent. `session.Start`,
   `Open` and `Resume` accept an API provider with the same Session, Turn,
   Event and Result shapes as the CLIs:
   - **Required configuration:** a `RuntimeHome`, the private durable
     directory that holds the session's state, and a `Restriction` whose
     `ToolHost` names the caller's tools and handler. No bridge is involved:
     the library calls the handler directly, so `Bridge` must be empty.
   - **The loop:** each turn calls the model through completion's
     OpenAI-compatible transport. The library runs each proposed call through
     the caller's handler, one at a time, keeping the tool host's closing-tool
     rule. It answers composed skill calls, appends the results, and repeats
     until the model answers without calls. A bounded number of model calls
     per turn keeps a looping model from running unattended.
   - **Tools:** only the caller's hosted tools and composed skills exist. The
     library writes every request itself, so the restricted tool surface holds
     by construction rather than by probe. `Support` says `RestrictTools` is
     composed and `Sandbox` unsupported: there are no native tools to
     sandbox.
   - **State:** each session keeps an append-only transcript under
     `RuntimeHome`, locked while a process has it open. The transcript holds
     messages, each tool call before it runs, each result after it returns, and
     per-response usage. A `Ref` names the engine, session ID, endpoint and a
     digest of model, effort, instructions and tool server, never a credential.
   - **Crash safety:** resuming after a crash never re-runs a tool. A call
     recorded without a result is answered in the history as having an
     unknown outcome, because its side effects may have happened, and the
     interrupted turn is reported as interrupted.
   - **Turn control:** interrupting cancels the request and any running
     handler. Steering is composed (interrupt, then a new prompt).
   - **Not yet:** compaction and streamed text deltas wait for their own
     increments. Chat Completions resends the whole history on every call,
     and usage reflects that.

7. **Skills the caller controls.** Tools an application relies on often come
   with skills (a `SKILL.md` plus the files it references). An engine's globally
   installed skills may stay detected and used, but wherever the library does
   not load them (restricted or sandboxed sessions, a Grok runtime home,
   reduced policies), the caller must be able to say exactly which skills are
   available:

   ```go
   type Skill struct {
       Name string // as the skill's SKILL.md front matter names it
       Dir  string // a directory holding SKILL.md and the files it references
   }
   type Skills struct {
       Global   GlobalSkills // Default (the mode's own rule), Include or Exclude
       Provided []Skill      // made available for this invocation only
   }
   ```

   `Support` gains `Skills` (caller-provided) and `GlobalSkills` features per
   engine and operation, and asking for either where it cannot be honoured is
   refused. Delivery follows each harness's own mechanism, verified against the
   installed CLI, and never writes into the operator's own homes:
   - **Claude:** `--plugin-dir` pointing at a library-written plugin that
     links the provided skills.
   - **Codex:** `$CODEX_HOME/skills` in a private runtime home. With the
     operator's own home, the request is refused, because
     `skills/list`'s per-cwd extra roots were not honoured (codex-cli 0.156.1).
   - **Grok:** `[skills] paths` in the config the library writes, or
     `--plugin-dir` for agent mode.

   Where an engine has no native mechanism, the library composes skills
   instead, so every engine and mode, including OpenAI-compatible endpoints,
   can use them:
   - **Constrained completion (every engine, including API gateways).** The
     system context carries an index of the provided skills (name and
     description from each `SKILL.md`). A reserved, read-only `load_skill`
     tool lets the model request a skill's `SKILL.md` or one of its files.
     Those calls return in `Result.SkillCalls`, apart from application tool
     proposals. `completion.AnswerSkillCalls` produces the tool-result messages
     from the provided directories, with paths contained and reads bounded, so
     the caller's loop appends them like any other tool result. The library
     never executes the caller's own tools.
   - **Native runs and sessions without a native mechanism.** The skill index
     and absolute paths go into the appended instructions, and the agent reads
     them with its own read tools. A restricted session has no native read
     tools, so the library hosts the same read-only skill tool through its tool
     channel.

   `Support` reports `Skills` as `native` where the harness loads skills itself
   and `composed` where the library does. `Skills.Delivery` defaults to
   automatic (native where verified, composed otherwise), and
   `SkillDeliveryComposed` forces the library's tools, for skills that should
   behave the same on every engine.

   **Scripts.** A skill may run its scripts only if `Skill.Scripts` is set.
   - **Native delivery:** the agent runs them with its own execution tools, and
     inside the proven sandbox when the session is sandboxed.
   - **Composed delivery:** a second reserved tool, `run_skill_script`, names a
     script inside the skill directory and an argument array. Nothing runs
     until the caller passes the call to the answering helper, which is the
     caller's authorization, as for every proposal. The helper runs the script
     with no shell, under process-tree containment, with a timeout, bounded
     output and a minimal environment, in a working directory the caller
     names.
   - **Caller-controlled execution:** an `Exec` hook lets an application run
     skill scripts in its own container or sandbox instead. Restricted
     sessions get the same behaviour through a ready-made handler for their
     tool host.

8. **Agents that can run and use what they build.** A QA or designer role
   that can only run `make check` answers "does it build?". To answer "does it
   work?", an agent must start the project and use it:
   - **Loopback networking.** `Sandbox.Loopback` lets a sandboxed session bind
     and reach loopback addresses only, so it can start a dev server and
     request it. The outside network stays closed. Each engine proves it before
     launch, like the rest of the sandbox, or the request is refused.
   - **The harness's own browser.** `Browser` turns on the browser integration
     each harness ships: Claude's `--chrome` (claude-in-chrome, which drives
     the operator's real Chrome through its extension, under the extension's
     site permissions) and Codex's `browser_use` features. Native integrations
     beat third-party automation in practice, so the library enables them
     rather than hosting its own. It is off by default and never implied by
     another option. A sandboxed session's proven tool surface then admits
     exactly the harness's browser tools beside its own. `Support(e, op,
     Browser)` reports which engines have one: Grok 1.0.41 does not, so it is
     refused there. A composed headless browser is a possible later fallback.
     Because Claude's browser is the operator's own, the capability reason says
     so, and the README recommends a dedicated Chrome profile for agents.

9. **Fail fast when the credential store cannot answer silently.** Claude Code
   keeps its login in the macOS keychain, so every Claude process the library
   starts (probes, sessions, completion, discovery, account inspection) reads
   it, and applications poll some of these. While the keychain answers
   silently this is invisible. When it cannot (locked, or wedged), each start
   raises its own prompt at the same time. Observed on 2026-09-28, with `gh`
   and GPG signing prompts in the mix, the pile-up wedged SecurityAgent ("Unapproved
   caller").
   - Before launching an engine whose login lives in the keychain, the library
     checks, without any interaction, that the keychain can answer. If it
     cannot, it refuses with a typed `keychain_unavailable` failure (preflight
     family, not retryable on its own terms), and never starts a process that
     would prompt.
   - The check must be proven not to prompt, on a throwaway keychain, never
     the operator's. It must stay CGO-free, and must be cheap enough to run
     before every launch, or be cached briefly.
   - The check belongs in `lib-agent-keyring`, the family's single OS
     secret-store package (already used by lib-agent-cli and lib-agent-mcp),
     as a non-interactive status: can the store answer silently, is it
     locked, or is it unavailable. Its own macOS `Get`/`Set` then refuse
     instead of prompting, so every agent CLI benefits. This library depends
     on its released version and does not reimplement keychain access.
   - Applications back off on `keychain_unavailable`. They stop polling quota
     and discovery until a check succeeds again, and show one "unlock your
     keychain" message rather than a failure per poll.
   - Out of scope: how a deployment provides credentials to other tools (`gh`
     tokens, GPG agents). The library only avoids adding prompts of its own.

A portable permission vocabulary for native runs is deliberately left out. It
would need a mapping for each engine, with a proof that no mapping widens
access, which is increment-5-sized work. Until then, engine option structs are
explicit, and `Support` reports what each engine can restrict.

## Release

The consumers depend on published versions (`AGENTS.md`), so each increment is
tagged. Increments 1–2 are `v0.6.0`, a breaking minor release while the module
is pre-1.0. crew-code-review moves from v0.2.0, and crew-assistant from v0.5.0.
