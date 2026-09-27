# API transports and gateway adapters

Proposed 2026-09-27. Revised 2026-09-27 after review against the `completion`
package and `AGENTS.md`; increment 1 is implemented in `completion/openai.go`.

## Decision

`lib-agent-harness` should support remote model APIs as **constrained
completion transports**. They belong beside the installed-CLI adapters because
callers need the same things from them: one configured provider, application
tool proposals the caller executes, usage that is measured or explicitly
unknown, and typed failures that exclude provider text.

The public boundary is the existing caller contract, not a claim that a remote
API is a native session. A caller chooses an installed CLI or a remote endpoint
in configuration and calls the same operation:

```go
reply, usage, err := completion.Complete(ctx, cfg, messages, tools)
```

`Message`, `Tool`, `ToolCall`, `Usage` and `*RequestError` stay the normalized
contract. An OpenAI-compatible endpoint is a new completion transport. It is not
a third kind of orchestration, not a `native` engine, and not a `session`
engine. Grok lives only in `native` and is unaffected by this work, as are the
Codex and Claude completion adapters.

The `completion` package has no capability type. For API transports, as for the
CLIs, a capability difference is exposed as a typed preflight refusal returned
before any credential is resolved or network request is made. No new
capability API is added until a caller needs to query support without calling.

## Provider configuration

`completion.Config` gains one branch. Existing fields and their defaults do not
change, and each engine ignores the other engines' location fields, as Codex
and Claude already ignore each other's binary and home:

```go
completion.Config{
    Engine: completion.EngineOpenAICompatible, // "openai-compatible"
    Model:  "provider/model",
    API: completion.APIConfig{
        BaseURL:     "https://gateway.example/v1",
        Dialect:     completion.OpenAIChatCompletions,
        Credentials: source, // completion.CredentialSource
    },
}
```

- **Dialect is required.** There is no default: an empty dialect is refused, so
  a later Responses dialect can never be reached, or avoided, by accident.
- **`CredentialSource` is `func(context.Context) (string, error)`**, returning
  one bearer token. A function rather than a struct or string means printing a
  `Config` with `%+v` cannot reveal a token. It is called once per `Complete`,
  after validation and after `BeforeRequest`, so a refused request never mints a
  token. Its error is never wrapped or retained; it is reported as a fixed
  `credential_unavailable` code (cancellation is still reported as
  cancellation). An empty token, or one containing control characters or
  spaces, is refused without being echoed. The library never caches a token and
  never reads ambient API-key environment variables.
- **The token is only sent as `Authorization: Bearer`.** Other schemes (for
  example an `api-key` header) need a typed option with a test.
- **`BaseURL`** must be absolute `https`, or `http` only to a loopback host
  (`localhost`, `127.0.0.0/8`, `::1`), with no user information, query or
  fragment. The dialect appends its path (`/chat/completions`) to it. Endpoint
  URLs never appear in errors.
- **No arbitrary headers or request fields.** A gateway option is added as a
  typed field with a test and a documented semantic, never as a pass-through
  map.
- **`Effort` is refused** for the API engine in increment 1 rather than dropped:
  `reasoning_effort` semantics differ across compatible providers. It is added
  as a typed, tested mapping when a caller needs it.
- **No ambient proxy.** The library's HTTP transport does not read
  `HTTPS_PROXY`/`HTTP_PROXY`, for the same reason CLI subprocesses get an
  allowlisted environment: process-wide settings must not route a caller's
  credential. A typed proxy or transport option is future work.

`Timeout` (default five minutes) bounds the whole request, headers and body.
`MaxContextBytes` (default 128 KiB) bounds the encoded request body.

## Increment 1: OpenAI Chat Completions, non-streaming

Scope: one `POST {BaseURL}/chat/completions` with `stream: false`, returning
text, tool proposals and terminal usage.

**Request.** `model`, `messages`, optional `tools`, and `stream: false`. Nothing
else is sent: no `n`, `tool_choice`, `parallel_tool_calls`, `max_tokens`,
`response_format` or vendor fields. The library adds no instructions; the
caller's messages are sent as given, because an API has no native tools to
suppress and the caller owns the prompt.

- Messages must use `system`, `user`, `assistant` or `tool`. A `tool` message
  needs `tool_call_id`, and only an `assistant` message may carry `tool_calls`,
  each a `function` call with an ID and a name. An assistant message with tool
  calls and no text sends `content: null`.
- Tools are validated exactly as for the CLIs: `function` type, unique non-empty
  names. An empty catalog omits `tools`. `strict` and `description` are sent
  only when set, and `parameters` only when non-nil.

**Transport.** One attempt. Redirects are refused, not followed (a redirect of
an authenticated POST changes where the credential and the body go). The
library never sets `Idempotency-Key`, which is what would let Go's transport
replay a POST. The response body is bounded (2 MiB, as for CLI output); an
error body is read to 64 KiB and only its structured `error.code`/`error.type`
are consulted against an allowlist.

**Terminal response.** Only a `200` with a JSON media type is a terminal
response. It must contain exactly one choice whose message role, if present, is
`assistant`. Content must be a string or null.

| `finish_reason` | Result |
| --- | --- |
| `stop` with no tool calls | text reply |
| `tool_calls` with at least one call | tool proposals, with any text |
| `stop` with tool calls, `tool_calls` with none, missing or unknown | `ambiguous_terminal_state` |
| `length` | `output_truncated`, no reply |
| `content_filter` | `content_filtered`, no reply |

A non-empty `refusal` is `model_refusal`, not text. Each tool call must be a
`function` call with a unique, bounded provider ID, a name in the supplied
catalog, and arguments that decode to a JSON object; at most 16 are accepted,
as for the CLIs. Any violation rejects the whole response: no partial
proposals are returned.

Tool call IDs are the provider's, not synthesized as the CLI adapters do: the
caller sends them back as `tool_call_id`, and the provider checks them against
the assistant message in history.

A response containing the request's credential anywhere is refused with
`credential_echoed`. An endpoint that reflects headers would otherwise place the
credential into a transcript.

**Usage.** `prompt_tokens` and `completion_tokens` must both be present,
non-negative and summable; if `total_tokens` is present it must equal their
sum. `prompt_tokens` already includes cached input. Anything else is unknown,
and unknown is not free. A `200` response that fails validation still returns
the usage it reported, beside the error and without a reply, because it may
have been billed. HTTP failures carry no usage. `ContextWindow` stays zero
because Chat Completions states no window.

**Failures.** Every error is a `*RequestError` with `Engine:
"openai-compatible"` and library codes only. `PhaseTransport` is added for
failures before a response status is known.

| Observation | Kind | Code | Retryable |
| --- | --- | --- | --- |
| caller cancellation | returns `context.Canceled` | | |
| deadline (caller or `Timeout`) | timeout | `deadline_exceeded` | no |
| connection/TLS/other transport failure | unknown | `transport_failed` | no |
| 3xx | unknown | `redirect_refused` | no |
| 401 / 403 | authentication / permission denied | `http_401` / `http_403` | no |
| 404 with `model_not_found` | model unavailable | `model_not_found` | no |
| other 404 | unknown | `http_404` | no |
| 400 with `context_length_exceeded`, or 413 | context limit | that code / `http_413` | no |
| 429 | rate limited | `http_429` | yes |
| 429 with `insufficient_quota` | unknown | `insufficient_quota` | no |
| 503 / 529 | unavailable / overloaded | `http_503` / `http_529` | yes |
| other status (including 500, 502, 504) | unknown | `http_<status>` | no |

A 500, 502 or 504 is not retryable: the gateway or upstream may still have
served, and billed, the request. `RetryAfter` is taken only from a
delta-seconds `Retry-After` header on a retryable status, bounded to one hour.
The library never retries.

Deferred from increment 1, each refused rather than approximated: streaming,
model discovery (`DiscoverModels` keeps refusing non-CLI engines),
`Effort`, structured output, the Responses dialect, gateway/vendor options,
serving-model metadata, and remote sessions.

## Capabilities by increment

“Native” means the provider protocol supports the operation. It does not mean
local-CLI semantics.

| Capability | Increment 1 | Later |
| --- | --- | --- |
| Text completion | native, from a verified terminal response | |
| Caller-hosted tool proposals | native, complete arguments only | |
| Usage | measured when the terminal response reports it; otherwise unknown | |
| Streaming | unsupported | increment 3 |
| Model discovery | unsupported (refused before any request) | increment 2 |
| Structured output | unsupported | increment 4 |
| Reasoning effort | unsupported (refused) | typed option on demand |
| Native tools / OS sandbox | unsupported, permanently for this transport | |
| Session resume, interrupt, steer | unsupported | separate decision |

The constrained-completion rule is unchanged: tools are proposed to the caller
and never executed by the library.

## Model discovery

`DiscoverModels(ctx, cfg) ([]ModelOption, error)` keeps its signature. For an
API transport it will perform an authenticated `GET {BaseURL}/models`, bounded
by the caller's context, the discovery timeout and a response limit, returning
no invented fallback models. A snapshot timestamp is the caller's observation
time; if a library-stated timestamp or serving metadata is needed, that is a new
function, not a change to this one.

- Configuring a provider never performs discovery.
- A caller may send a configured model without discovering first.
- Interfaces run discovery off their foreground path and keep their last
  successful result when a refresh fails.
- Authorization, entitlements, routing and outages can change the result, so it
  is advisory, not a guarantee that a later request succeeds.

## Vercel AI Gateway

Vercel AI Gateway is an OpenAI-compatible endpoint, not an engine or package. A
caller uses increment 1 with the Gateway's base URL and a credential source; no
Gateway-specific code is needed for that. Its Responses endpoint is reachable
only through the Responses dialect once it exists.

Gateway-only controls (provider ordering, allowlists, fallbacks, provider
timeouts) become a small typed `GatewayOptions` field only when a caller needs
them. Reporting the serving model or provider needs somewhere to put it in the
result; that is an additive API decision for that increment, not something to
smuggle into `Message.Content` or `Usage`. The harness never retries a request
a gateway may already have routed or retried.

## Sessions are a later, separate decision

Do not add a remote endpoint to `session.Options`. A remote API has no evidence
of a CLI-owned working directory, a native tool surface or an OS sandbox. It may
support a provider conversation ID, but its transcript and resume semantics are
application state, not a native session.

If a real caller needs long-lived remote conversations, design a separate
remote-session adapter after the completion transport is proven. It may share
`StartTurn`, event and capability shapes, but it must report its own
persistence, cancellation and tool-execution guarantees, and must never satisfy
a caller that requires a verified native sandbox or restricted tool surface.

## Delivery plan

1. **Done.** Non-streaming Chat Completions behind `completion.Complete`, as
   specified above: typed configuration, credential source, bounded request and
   response, complete terminal parsing of text, tool proposals and usage,
   sanitized failures, and cancellation. Tests use an injected
   `http.RoundTripper` only; nothing contacts a network.
2. Authenticated `GET /models` discovery with bounded parsing, pagination
   limits, cancellation and credential-redaction tests.
3. Streaming Server-Sent Events for the same dialect, with a pinned parser that
   rejects incomplete tool arguments, malformed events, a missing `[DONE]` or
   final chunk, and ambiguous terminal state, and that reads usage only from the
   terminal chunk (`stream_options.include_usage`). The non-streaming terminal
   rules above remain the definition of a complete result.
4. Structured output, only after request shape and terminal validation are
   verified for the dialect.
5. The OpenAI Responses dialect, only after fixture and manual live-compat
   evidence establish its tool, stream, structured-output and usage semantics.
6. Gateway preset options and serving metadata, when a caller needs provider
   selection, fallback or serving-provider observability.
7. Remote sessions, only after a concrete application requires them.

## Acceptance criteria

- Existing Codex and Claude completion behaviour, and all `native` (including
  Grok) and `session` behaviour, is unchanged.
- No API key, authorization header or endpoint credential reaches a prompt,
  transcript, error, test fixture or persisted state. The package has no
  diagnostic hook for API transports; one added later is bounded and sanitized.
- Every network operation has a context, a timeout, request and response size
  limits, a refuse-redirects policy, a single attempt, and sanitized errors.
- A caller gets the same request, result and tool-proposal shapes for a CLI and
  an OpenAI-compatible endpoint; unsupported options are refused before any
  request rather than silently dropped.
- Discovery failure invents no catalogue and does not block an explicitly
  configured model from being attempted.
- Unit tests use an injected HTTP transport and synthetic JSON (later SSE)
  fixtures with dummy credentials; optional live checks are manual and never run
  in CI. The API transport has no process or platform dependency, so its tests
  run on Windows too.
