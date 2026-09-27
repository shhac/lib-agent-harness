# Constrained completions

`completion.Complete` invokes a local Codex, Claude or Grok CLI, or a configured
OpenAI-compatible Chat Completions endpoint, once and returns a `Result`: a
message with optional proposed application tool calls, and the shared
`harness.Usage`, `harness.Cost` and stated context window. It never executes
proposed tools. The caller owns authorization, execution, conversation history,
retries, and budgets.

`Config.Provider` is the shared `harness.Provider`. Its `Engine` is
`harness.Codex`, `harness.Claude`, `harness.Grok` or `harness.OpenAICompatible`;
the CLI engines read `Provider.CLI` (`Binary`, `Home`) and the API engine reads
`Provider.API`. Before anything else, an engine for which
`harness.Support(engine, harness.Complete, harness.Available)` is not usable is
refused with `unsupported_engine`. A provider whose
`Problem()` is non-empty, such as one setting the half its engine does not read
(`api_config_for_cli_engine`, `cli_config_for_api_engine`), is then refused
with that code before any process, credential or request.

For `harness.OpenAICompatible`, set an explicit `API.Dialect`, an absolute HTTPS
`API.BaseURL` (HTTP is allowed only to loopback), and an `API.Credentials`
function, or `API.Unauthenticated` for a loopback server that takes none. The function yields one bearer token after local validation and
`BeforeRequest`; it is not stored in `Config`, wrapped in errors, or read from
an ambient API-key environment variable. The initial dialect is
`harness.OpenAIChatCompletions`: a single non-streaming request containing the
model, conversation, optional function catalog, optional reasoning effort and
optional output cap (`max_completion_tokens`, see below).
Effort requires `API.EffortParameter` (`harness.EffortReasoningEffort` or
`harness.EffortReasoningObject`), because compatible endpoints read it from
different fields. It rejects redirects, arbitrary headers and vendor fields, and does not
retry. Responses must have one complete terminal choice; usage is known only
when the response supplies consistent token counts. Streaming, structured
output and remote sessions are currently refused rather than silently
approximated.

Select `Config.Provider`, `Model`, and optional `Effort`; set `Provider.CLI`'s
binary and native login home paths when needed. The library does not copy credentials or fall back
to API billing. For CLI engines, Claude retains native login/keychain resolution,
including the `USER` environment variable. Ambient API keys and integration
credentials are not forwarded. Native login access remains available to the CLI.

Both adapters disable native tools, hooks, custom instructions, external MCP
servers, and session persistence. Before inference they make a synthetic request
to a local rejecting HTTP server with dummy provider credentials to verify the
actual tool/instruction surface. These probes perform no inference. An unknown
or incompatible CLI fails closed. Codex homes containing nonempty global
`AGENTS.md` or `AGENTS.override.md` are rejected because this invocation mode
cannot reliably disable those instructions.

Grok requires `WorkDirRoot`. Its runs use a private runtime home at
`<WorkDirRoot>/grok-home` holding configuration this library writes, and share
only `auth.json` with the operator's Grok home (`Provider.CLI.Home`, else
`GROK_HOME`, else `~/.grok`), so the operator's MCP servers, hooks, plugins and
rules are never read. A digest record decides which login copy wins: an
operator login change or logout always does; a refresh Grok made in the runtime
home is kept while the source is unchanged, and is written back after the run.
Each run gets a private `HOME`, temporary directory and empty working
directory, and the prompt is passed in a file, never on the command line.
Native tools are removed with `--tools=read_file
--disallowed-tools=read_file,search_tool,use_tool --no-subagents
--disable-web-search`, the system prompt is replaced, and the action envelope
is requested with `--json-schema`. Before the first launch of each binary
(path, file identity and version) and flag set, the same launch runs against a
disposable home whose only model is a loopback provider that refuses inference
(declared like Grok's own models: Responses API, backend search, the requested
effort). The request must carry no tools, our system text, exactly one vendor
context message (`<user_info>` plus Grok's built-in `<user_rules>` and nothing
else), our prompt, schema, model and effort; Grok's own session-title request is
tolerated but not relied on. Only proven configurations are cached, in process.
Every real run is judged again: a non-empty native tool catalog or any native
tool call in its stream stops it at once (`unexpected_native_tool_catalog`,
`unexpected_native_tool_call`), and its persisted transcript must repeat the
probe's vendor context with no system or user message added after the prompt
(`unexpected_native_instructions`), since an authenticated run may receive
remote settings the probe cannot. These are request failures, since the
request may have started. The persisted session is deleted after each run.
Grok completion is refused on Windows (`grok_platform_unsupported`) because the
runtime home cannot be made owner-only there. Effort is passed through as
`--reasoning-effort`; `catalog.Discover` lists what a model accepts.

`BeforeRequest` runs only after these non-billable checks and immediately before
the inference invocation. Its error prevents that invocation. It gates one CLI
invocation, which is the unit this library controls; a CLI or provider may make
more than one upstream request inside it, so this is not a per-provider-request
gate. Failures are not retried, since usage and external effects can be
uncertain. CLI diagnostics are not included in returned errors. The output
stream is bounded. Claude may invent a tool that is not available and then
recover after the CLI rejects it. Recovery is accepted only with a restricted
initial tool catalog, unique call IDs, exact matching CLI “No such tool available”
receipts, and a valid final structured response. Generic errors, unacknowledged
calls, duplicate receipts, and calls after completion fail closed; no application
proposal from a rejected response is returned. Terminal usage is retained even
when response validation fails. Unexpected native tool catalogs/calls have
separate diagnostics. Other unexpected native tool events or malformed structured
responses are rejected.

Usage comes from one definition for every invocation, successful or not, so the
two paths cannot disagree about the same provider's accounting. Only an
authoritative terminal report counts — Claude's `result` event, Codex's
`turn.completed`, or the Chat Completions response's `usage`. A report that
cannot be fully parsed, that is one of several terminal reports, or that is
absent, incomplete, negative, inconsistent or too large to sum is unknown
(`Known` false); a report of explicit zeros is a measurement and stays known.

`Usage.Input` is every prompt token, cached or not; `CacheRead` and
`CacheWrite` are parts of it, and `Reasoning` is part of `Output`.
`CacheKnown` is set only when the provider actually reported the split, so
zero cache figures without it mean unreported, not uncached:

- Claude: `Input` is `input_tokens` plus `cache_read_input_tokens` and
  `cache_creation_input_tokens`; the split is known when both cache fields are
  present. `Cost` is the result's `total_cost_usd` when stated.
- Codex: `input_tokens` already includes `cached_input_tokens`, which becomes
  `CacheRead` when present; `reasoning_output_tokens` becomes `Reasoning`. A
  part larger than its whole makes the report unknown. Codex states no cost.
- Grok: the single `end` event. `input_tokens` is uncached input, so `Input`
  adds `cache_read_input_tokens` and `cache_creation_input_tokens` back; the
  split is known when both are present. `Cost` is `total_cost_usd_ticks`
  (10^-10 USD), else `total_cost_usd`. `usage_is_incomplete` makes both
  unknown and `cost_is_partial` makes the cost unknown; an `error` event's
  spend counts only when `end` reports none. Whether Grok's own session-title
  request is included is unverified. No context window is stated.
- Chat Completions: `Input` is `prompt_tokens` and `Output` is
  `completion_tokens`, which must agree with any `total_tokens`. Optional
  `prompt_tokens_details.cached_tokens` and
  `completion_tokens_details.reasoning_tokens` fill `CacheRead` (with
  `CacheKnown`) and `Reasoning`. No cost is stated.

`Result.ContextWindow` is the provider's stated window, in tokens, for the
model that served the request, read from the same terminal report; zero is
unknown, never a guess. Claude states it in the result's `modelUsage`, keyed by
the model the latest assistant message names (a sole entry is used when that
name does not match). Codex exec's JSON stream and Chat Completions responses
state no window, so they leave it zero.

A failed invocation that may have been billed therefore still returns a
`Result` carrying what the provider said it consumed and cost, alongside the
original typed error, with a zero `Message`: never an action proposal.

Each call uses a private, disposable working directory. When `WorkDirRoot` is
provided, it must be a canonical existing directory; a private `model-runs`
child is created without following child symlinks. The call's directory is
removed on success and error. An empty root uses the operating system temporary
directory. Do not use a project directory as the root. No project workspace is
provided to the CLI. On Windows the root (or the OS temporary directory when
no root is provided) must already have a private ACL; child directories inherit
that ACL. Windows `chmod` only changes file attributes and does not grant
owner-only access.

Model discovery lives in [catalog](../catalog/README.md).

The unit suite uses synthetic CLI responses and local rejecting servers. Optional
installed-Codex protocol tests are explicitly gated; they also use a local dummy
provider. No test needs a paid model call or production account data.

The native environment is an explicit OS-context allowlist. On Windows this
includes `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `SystemRoot`, `COMSPEC`, and
`PATHEXT`; temporary paths include `TMP` and `TEMP` as well as `TMPDIR`. Provider
secrets and arbitrary process overrides remain excluded. Dummy probes rebase
home, config/cache, and temporary directories and remove native user identity.
The Codex capability probe retains only its explicitly selected `CODEX_HOME` to
verify that home's instruction boundary; its provider authentication remains a
dummy local key. The bundled model catalog uses a disposable Codex home too.

## Output token cap

`Config.MaxOutputTokens` caps what the model may generate for each model
request, reasoning included. Zero leaves the provider's default; a negative
value is refused (`invalid_limits`). An engine accepts a cap only through a
mechanism verified to reach the request; otherwise a non-zero cap is refused
before any process or request with `max_output_tokens_unsupported`, a
capability failure, and never silently dropped:

- OpenAI-compatible: sent as `max_completion_tokens`, OpenAI's current Chat
  Completions field. The deprecated `max_tokens` is not sent (OpenAI's
  reasoning models do not accept it), so a gateway that reads only
  `max_tokens` will not apply the cap. A reply cut off at it finishes
  `length` and fails with `output_truncated`.
- Claude: passed as `CLAUDE_CODE_MAX_OUTPUT_TOKENS`, which Claude Code turns
  into each request's `max_tokens`, lowering a cap above the model's own
  limit. The capability probe requires every request it receives to carry
  `max_tokens` between 1 and the cap (`probe_changed_max_output_tokens`
  otherwise). An ambient value is never forwarded. A cap below the model's
  extended-thinking budget can be refused by the provider.
- Codex: refused. `codex exec` 0.156.1 sends no output limit for any
  configuration key tried (`model_max_output_tokens`, `max_output_tokens`,
  `model_max_completion_tokens`), and its model catalog has no such field.
- Grok: refused. Grok's `max_completion_tokens` (under `[models]` or
  `[model.<id>]`) reaches the probe model's request as `max_output_tokens`,
  but a real run uses Grok's catalog model, whose own catalog value
  overrides the `[models]` default and is not visible to a local probe, and
  neither the stream nor the transcript records the cap.

## Skills

`Config.Skills` makes caller-provided skills available to the model, for every
engine. Constrained completion has no native tools, so no harness loads skills
here: `harness.Support(engine, harness.Complete, harness.ProvidedSkills)` is
`composed`, and the library composes them. `Skills.Global` must be Default or
Exclude; Include is refused (`global_skills_unsupported`, a capability
failure) because installed skills are never loaded. `Skills.Delivery` may be
Auto or Composed; both mean composed here.

Each `harness.Skill` names an absolute directory holding `SKILL.md`, whose YAML
front matter gives `name` (equal to `Skill.Name`: lowercase letters, digits and
hyphens, at most 64) and `description` (at most 1024 characters). The front
matter reader is a small strict subset: plain, quoted, continued or block
scalars for those two fields; other keys are allowed and ignored. At most 64
skills, no duplicate names, `SKILL.md` at most 256 KiB of UTF-8 text inside the
directory. A bad skill is refused before any process or request with a fixed
code (`skill_dir_invalid`, `skill_manifest_missing`,
`skill_front_matter_invalid`, `skill_name_mismatch`, …).

The model sees an index of the provided skills (name and description) in the
system context, appended to a leading system message or as a new one, and the
library's tools after the caller's: `load_skill` `{"skill", "file"?}` reads
`SKILL.md` or a file the skill references, and `run_skill_script` `{"skill",
"script", "args"?}` is offered only when a provided skill sets `Scripts`. For a
CLI engine both travel in the rendered payload beside `messages` and
`available_tools`, so the proven system instructions are unchanged; an
OpenAI-compatible request carries them as function tools. While skills are
provided, a caller tool with either name is refused
(`skill_tool_name_reserved`).

Skill calls are proposals. They stay in `Message.ToolCalls`, in the model's
order, so the assistant message is appended to history as it is and every
later tool message has its matching call (Chat Completions requires that).
`Result.SkillCalls` lists them again; `Result.ApplicationCalls()` is the rest.
A skill call whose arguments are not the tool's exact shape fails the
response (`invalid_skill_call`) with no proposal. `AnswerSkillCalls` answers
the skill calls it is given, skipping others, one `tool` message each: a file's
text, a script's JSON result, or a fixed `<tool> error: <code>` text for an
unknown skill, a path outside the skill (`..` or a symbolic link), a missing,
non-regular, oversized (over 256 KiB) or non-text file, and so on. Nothing
read from a file or script ever enters an error value.

```go
result, err := completion.Complete(ctx, cfg, history, tools)
// handle err
history = append(history, result.Message)
history = append(history, completion.AnswerSkillCalls(ctx, result.SkillCalls, cfg.Skills,
    completion.SkillRunOptions{WorkDir: scratch})...)
for _, call := range result.ApplicationCalls() {
    history = append(history, app.Execute(call)) // role "tool", ToolCallID call.ID
}
```

Answering is the caller's authorization: a script runs only when its call is
passed to `AnswerSkillCalls`, one at a time, and only for a skill with
`Scripts` set and a non-empty `SkillRunOptions.WorkDir`. The library's runner
executes the script directly, never through a shell: it must be a regular
file inside the skill with an execute permission (the operating system reads
any `#!` line), `args` is its argument vector, the working directory is
`WorkDir`, and the environment is only the parent's `PATH`, `HOME`, `TMPDIR`,
`LANG` and `LC_*` plus `SkillRunOptions.Env`. It runs in its own process tree,
ended as a whole by the timeout (one minute by default, at most ten) or by
cancellation. Stdout and stderr are each kept to 64 KiB and marked truncated
beyond. The result is `{"exit_code", "timed_out", "stdout",
"stdout_truncated", "stderr", "stderr_truncated"}`; a non-zero exit is a
result, not an error. The library's runner refuses on Windows. Set
`SkillRunOptions.Exec` to run the checked `SkillCommand` in the application's
own container or sandbox instead; its error is answered as `script_failed`.

## Failure classification

Use `harness.ErrorFacts(err)` to classify a failure in the vocabulary every
mode shares, or `errors.As(err, &requestError)` with `*completion.RequestError`
to inspect `Cause` (a `harness.Cause`), `RetryAfter` and `Retryable()`.
`Engine` (a `harness.Engine`), `Phase` (preflight, process, transport,
response), `Code` and optional `ExitCode` provide safe diagnostics without
retaining raw provider or subprocess text.

`HarnessFacts` reports `Operation` `harness.Complete` and a `Family`:
`harness.FailureCapability` for refusals the same configuration will always
repeat (`unsupported_engine`; native tools advertised or attempted,
`unexpected_native_tool*`; capability-probe mismatches such as
`probe_unexpected_tools` or `probe_mismatch`; `missing_effort_catalog`,
`unsupported_effort`, `api_dialect_unsupported`,
`api_effort_parameter_unsupported`, `global_skills_unsupported`,
`skills_unsupported`, `max_output_tokens_unsupported`), `harness.FailurePreflight` for other
preflight codes, `harness.FailureProcess` for the process phase, and
`harness.FailureRequest` for the transport and response phases. `Code` is an allowlisted native enum
or library code; an unknown native value is omitted. Exit status is only present
when observed from the subprocess. These fields do not establish whether quota
was consumed. Only explicit overload, rate-limit, and
service-unavailable failures without partial response output are retryable.
Authentication, permission denials, unavailable models, structured-output
exhaustion, context limits, unknown failures, timeouts, transport loss,
malformed output and capability probes never authorize a retry. The caller owns
retry budgets and scheduling; no request is retried here. Classification does
not establish that the failed request consumed no quota. `RetryAfter` is zero
when the CLI supplies no trustworthy delay (currently both completion adapters).

Claude classification requires a typed assistant error and terminal failed
result. Codex exec exposes only a message in its terminal error envelope, so the
adapter recognizes a narrow set of canonical native error formats: exact
capacity/context failures and anchored HTTP 429/503/529/401/403 status formats. Any
item output, successful completion, malformed/unknown event or unrecognized
format prevents transient classification. Provider prose is never searched for
keywords or retained in public errors. Sources:
[Codex error formatting](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/error.rs),
[Codex exec envelope](https://github.com/openai/codex/blob/main/codex-rs/exec/src/exec_events.rs),
[Claude native message types](https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/types.py).

Claude terminal diagnostics recognize structured-output exhaustion, context
limits, model-not-found and account permission errors even after partial output;
they never make such partial responses retryable. Timeouts use
`harness.CauseTimeout` and preserve `errors.Is(err, context.DeadlineExceeded)`.
Cancellation preserves `context.Canceled`. Missing Codex catalog models fail at
preflight with `harness.CauseModelUnavailable`; this is not a claim about remote
model availability.
