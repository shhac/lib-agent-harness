# Model catalogs

`catalog.Discover(ctx, provider)` lists the models an engine offers, with their
reasoning efforts and defaults, without starting a conversation or an inference
request. It runs where `harness.Support(engine, harness.Models,
harness.Available)` is usable; a provider whose `Problem()` is non-empty is
refused before any process or request.

| Engine | Source | Efforts | Context window |
| --- | --- | --- | --- |
| Codex | `app-server` `initialize` then paginated `model/list` (hidden models left out) | stated | not stated |
| Claude | stream-json `initialize` control request, under the restricted flag set completion uses | stated per model where Claude lists them | not stated |
| Grok | `grok agent --no-leader stdio`: one ACP `initialize`, reading `result._meta.modelState` | stated where `supportsReasoningEffort` is present | `totalContextTokens` |
| OpenAI-compatible | `GET {BaseURL}/models`, one page | never listed | `context_window` (Vercel AI Gateway) or `context_length` (OpenRouter) when numeric |

Grok's `session/new` also reports models, but it runs the user's hooks and
creates a session, so it is never sent. The Grok process is stopped as soon as
the `initialize` reply arrives.

`Model.EffortsKnown` says whether the engine stated which efforts a model
accepts. When it is false, `Efforts` is nil and means only that the engine did
not say, not that the model takes none. `DefaultEffort`, `IsDefault` and
`ContextWindow` are likewise only what the engine stated.

CLI children get the same allowlisted environment as `completion`: operating
context and the selected login home (`CODEX_HOME`, `CLAUDE_CONFIG_DIR`,
`GROK_HOME`), never provider API keys. Grok also gets the reduced-telemetry
overrides. Each child runs in an empty private directory with stderr discarded.
An API request uses only the provider's credential source, ignores ambient
proxies, refuses redirects, and fails if the endpoint echoes the credential.

Discovery is bounded to 15 seconds within `ctx`, to 4 MiB of output (1 MiB per
CLI line), to 10 Codex pages, and to 2000 models. Exceeding a bound is an
error, never a truncated list. Failures are `*catalog.Error`, which implements
`harness.Factual` with `Operation: harness.Models` and a fixed `Code`; no CLI
output, provider prose, path or credential is retained. Cancelling `ctx`
returns `ctx.Err()`; discovery's own time bound is a typed `deadline_exceeded`
that still matches `context.DeadlineExceeded`.
