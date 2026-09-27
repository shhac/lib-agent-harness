# Constrained completions

`completion.Complete` invokes a local Codex or Claude CLI, or a configured
OpenAI-compatible Chat Completions endpoint, once and returns a `Result`: a
message with optional proposed application tool calls, and the shared
`harness.Usage`, `harness.Cost` and stated context window. It never executes
proposed tools. The caller owns authorization, execution, conversation history,
retries, and budgets.

`Config.Provider` is the shared `harness.Provider`. Its `Engine` is
`harness.Codex`, `harness.Claude` or `harness.OpenAICompatible`; the CLI
engines read `Provider.CLI` (`Binary`, `Home`) and the API engine reads
`Provider.API`. Before anything else, an engine for which
`harness.Support(engine, harness.Complete, harness.Available)` is not usable,
Grok included, is refused with `unsupported_engine`. A provider whose
`Problem()` is non-empty, such as one setting the half its engine does not read
(`api_config_for_cli_engine`, `cli_config_for_api_engine`), is then refused
with that code before any process, credential or request.

For `harness.OpenAICompatible`, set an explicit `API.Dialect`, an absolute HTTPS
`API.BaseURL` (HTTP is allowed only to loopback), and an `API.Credentials`
function, or `API.Unauthenticated` for a loopback server that takes none. The function yields one bearer token after local validation and
`BeforeRequest`; it is not stored in `Config`, wrapped in errors, or read from
an ambient API-key environment variable. The initial dialect is
`harness.OpenAIChatCompletions`: a single non-streaming request containing the
model, conversation, optional function catalog and optional reasoning effort.
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
`api_effort_parameter_unsupported`), `harness.FailurePreflight` for other
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
