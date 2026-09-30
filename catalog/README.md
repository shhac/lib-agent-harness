# Model catalogs

`catalog.Discover(ctx, provider)` lists the models an engine offers, with their
reasoning efforts and defaults, without starting a conversation or an inference
request. It runs where `harness.Support(engine, harness.Models,
harness.Available)` is usable; a provider whose `Problem()` is non-empty is
refused before any process or request.

| Engine | Source | Efforts | Context window |
| --- | --- | --- | --- |
| Codex | `app-server` `initialize` then paginated `model/list` (hidden models left out) | stated | `context_window` from `debug models --bundled`: the binary's own catalog, which the account's service may differ from |
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

`Model.Parameters` and `Model.ParametersKnown` work the same way for the
request parameters an OpenAI-compatible endpoint lists per model in
`supported_parameters` (OpenRouter does). They are taken only as an array of at
most 128 non-empty strings of at most 64 bytes each, repeats dropped and order
kept; `[]` is known and empty. Null, a missing field, another type or a longer
list leaves that model's parameters unknown, never failing the catalog. CLI
engines never list them. Each parameter is included in the credential-echo
check. `SupportsTools(m)` reports whether `tools` is listed, and whether that
is known; `session.Options.CatalogModel` uses it to refuse a model without tool
calling before launch.

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
