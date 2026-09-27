# API transports and gateway adapters

Proposed 2026-09-27.

## Decision

`lib-agent-harness` should support remote model APIs. They belong beside the
installed-CLI adapters because callers need the same things: one configured
provider, constrained tool proposals, structured output, streaming, usage, and
explicit capability evidence.

The public boundary is a **common caller contract**, not a claim that a remote
API is a native session. A caller chooses either an installed CLI or a remote
endpoint in configuration, then calls the same high-level operations:

```go
reply, usage, err := completion.Complete(ctx, cfg, messages, tools)
models, err := completion.DiscoverModels(ctx, cfg)
```

The existing `completion` types (`Message`, `Tool`, `ToolCall`, `Usage`) remain
the normalized contract. An OpenAI-compatible endpoint is a new completion
transport, not a third kind of orchestration and not a native harness engine.

## Provider configuration

Add an API-provider branch to `completion.Config`, without changing the
existing Codex and Claude fields or their defaults. The eventual public shape
should carry these concepts (exact Go names are an implementation detail):

```go
completion.Config{
    Engine: "openai-compatible",
    Model:  "provider/model",
    API: completion.APIConfig{
        BaseURL: "https://gateway.example/v1",
        Dialect: completion.OpenAIChatCompletions,
        Auth:    credentialSource,
    },
}
```

`Auth` is an opaque caller-supplied credential source, not a `string` token in
the config. It is resolved as late as possible for one HTTP request and is
never included in returned errors, diagnostic hooks, model messages, durable
references, or test fixtures. A caller that has its own secret manager can
inject a short-lived token source. The library must not read ambient API-key
environment variables by default.

The endpoint, dialect, selected model, request limits, and safe public headers
are configuration. Provider-specific headers and unsupported arbitrary request
fields are not an unvalidated escape hatch. If a real gateway needs an option,
add a typed option with a test and a documented semantic.

## OpenAI-compatible first

Implement the broadest useful dialect first: OpenAI Chat Completions with
streaming Server-Sent Events, text, application tool calls, and terminal usage.
Many gateway products implement it, including Vercel AI Gateway. Keep the
Responses API as a second explicit dialect: it has a distinct wire format and
must not be silently treated as Chat Completions merely because an endpoint is
called “OpenAI-compatible”.

Each dialect declares capabilities independently:

| Capability | Initial API behaviour |
| --- | --- |
| Text completion | native after a complete terminal response is observed |
| Caller-hosted tool proposals | native when the dialect returns complete tool arguments |
| Streaming | native when the selected dialect's stream parser is pinned |
| Structured output | unsupported until request and terminal validation are verified for that dialect |
| Usage | measured only when the terminal response reports it; otherwise unknown |
| Model discovery | asynchronous snapshot, never inferred |
| Native tools / OS sandbox | unsupported |
| Native session resume, interrupt, steer | unsupported in phase one |

“Native” here means the provider protocol supports the operation, not that it
has local-CLI semantics. The constrained-completion rule remains unchanged:
tools are proposed to the caller and never executed by the library.

## Model discovery

`completion.DiscoverModels(ctx, cfg)` remains the API. For an API transport it
performs an authenticated remote request, subject to a short caller context and
the transport timeout. It returns a timestamped model-catalog snapshot with no
invented fallback models.

Discovery is separate from `Complete`:

- Opening/configuring a provider never performs network discovery.
- A caller may send a configured model without first discovering models.
- User interfaces run discovery off their foreground path and retain their last
  successful snapshot when a refresh fails.
- Authorization, account entitlements, gateway routing, and provider outages
  can all change the result, so a snapshot is advisory rather than a guarantee
  that a later request will succeed.

## Vercel AI Gateway

Vercel AI Gateway is initially an OpenAI-compatible endpoint preset, not its
own engine or package. The caller selects the ordinary API transport and uses
the Gateway base URL and a credential source. Its documented Chat Completions
and Responses endpoints are supported only through the corresponding explicit
dialect.

Gateway-only controls such as provider ordering, allowlists, fallbacks, and
provider timeouts should be added as a small typed `GatewayOptions` field only
when a caller needs them. The normalized result preserves public serving-model
and provider metadata when present, rather than pretending that configured and
served models are always identical. The harness itself never retries a request
that a gateway may already have routed or retried.

## Sessions are a later, separate decision

Do not add a remote endpoint to `session.Options` in this work. A remote API
has no evidence of a CLI-owned working directory, native tool surface, or OS
sandbox. It may support a provider conversation ID, but its durable transcript
and resume semantics are application state, not a native session.

If a real caller needs long-lived remote conversations, design a separate
remote-session adapter after the completion transport is proven. It may share
`StartTurn`, event, and capability shapes, but must report its own persistence,
cancellation, and tool-execution guarantees. It must never satisfy a caller
that requires a verified native sandbox or restricted native tool surface.

## Delivery plan

1. Add the OpenAI Chat Completions transport behind `completion.Complete`, with
   an injected HTTP client and synthetic response/SSE fixtures. No real API,
   credential, or gateway is contacted by tests.
2. Add authenticated `GET /models` discovery with bounded response parsing,
   snapshot metadata, cancellation, and credential-redaction tests.
3. Add precise terminal parsing for tool calls and usage. Reject incomplete
   tool arguments, malformed streams, ambiguous terminal states, and unknown
   structured-output modes rather than guessing.
4. Add an OpenAI Responses dialect only after fixture and live-compat evidence
   establish its tool, stream, structured-output, and usage semantics.
5. Add the Vercel Gateway preset and typed routing metadata only when a caller
   needs provider selection, fallback, or serving-provider observability.
6. Revisit remote sessions only after a concrete application requires them.

## Acceptance criteria

- Existing Codex, Claude, and Grok behaviour remains unchanged.
- No API key, authorization header, or endpoint credential reaches a prompt,
  transcript, error, diagnostic callback, test fixture, or persisted state.
- Every network operation has a context, a size limit, redirect policy, and
  sanitized errors.
- A caller gets the same completion request/result/tool proposal shapes for a
  CLI and OpenAI-compatible endpoint, with capability differences exposed
  rather than hidden.
- Discovery failure preserves no invented catalogue and does not block an
  explicitly configured model from being attempted.
- Unit tests use an injected HTTP transport and synthetic SSE/JSON fixtures;
  optional live checks are manual and never run in CI.
