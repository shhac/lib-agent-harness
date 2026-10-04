# Engine adapters

Status: accepted, implementing in stages (2026-10-04). Revised after review: stage order, invariants 1, 2 and 4, and the Go shape.

## Why

Adding an engine should mean adding that engine's files and one registration,
with the compiler pointing at anything missing. Today it means editing most of
`session/`: a structure audit found roughly 166 places across 19 files that
branch on the engine (transport dialect, launch arguments, environment policy,
event handling, per-turn and per-session state, telemetry, sandbox proofs,
login sharing). Command Code's integration showed the cost: its environment
handling had to be found and patched in several places, and a missed branch is
silent.

The library already has three working precedents to generalise rather than
invent:

- `catalog/cli.go`: a per-engine struct of functions (`cliEngine`) looked up
  once from a map keyed by `harness.Engine`.
- `native/native.go`: the `transcoder` interface, with one implementation per
  engine selected once in `NewStream`.
- `session/acp.go`: shared helpers (refusals, initialize parsing) that Grok
  and Command Code use instead of copying. It is not yet a base adapter.

## Shape

An immutable registry of per-engine entries, keyed by `harness.Engine`, plus
small interfaces for the parts that hold state. A registry entry never holds
mutable session state.

Registry entry (struct of functions and data, like `catalog.cliEngine`):

- descriptor data: home variable, home directory suffix, default binary,
  composed-steer reason;
- the wire dialect factory;
- option normalisation and launch-argument functions;
- a session-driver factory.

Stateful contracts, each small, so an engine implements only what it offers
and shared code refuses the rest with a named reason:

- `dialect`: frame envelope, reply parsing, recognising server requests and
  encoding the engine's answer or refusal;
- `sessionDriver`: typed per-session engine state, initialization, startup
  checks, session notifications, and a factory for turn drivers;
- `turnDriver`: typed per-turn state, start, interrupt, steer, event
  handling, streamed telemetry;
- `telemetryReader`: account, quota and context reads;
- `prover`: sandbox probe and live check, restriction probe.

A concrete turn driver (`codexTurn`, `claudeTurn`, an ACP turn) holds typed
pointers to its session driver and the common `Turn`, so handlers use their
own fields directly; shared code stores the behavioural interface, never
`any`. The existing lock boundaries stay: `eventMu` serialises notification,
replay and ACP terminal handling; session and turn fields keep their mutexes.

Grok and Command Code compose new ACP helpers (framing, initialize parsing,
prompt waiting, cancellation, tool-content decoding) alongside `acp.go`'s
shared refusals. Go embedding gives no virtual dispatch, so a helper that
needs engine behaviour takes an explicit callback. Permission selection,
resume checks, configuration and terminal accounting stay engine-specific
(Command Code's before-and-after resume listing is not Grok's).

A function registry does not make omissions compile errors, so interface
implementations carry completeness where it matters, and a test validates
every registry entry for nil functions.

`harness.Describe` (stage 7) lives below `session`, in the root package, so
`catalog`, `completion` and `native` can use it without an import cycle. It
shares static facts only, not launch policy: native deliberately inherits the
caller's environment under its documented defaults, unlike session filtering.

## Invariants the shared code keeps

These stay in shared orchestration that calls the engine's functions. An
engine cannot skip or reorder them:

1. Prove before any credentialed process. Probes run disposable, with dummy
   credentials. The order differs by mode and is kept exactly: a restricted
   launch opens its host (which permits discovery and refuses execution) and
   copies the Codex login before probing; a sandboxed launch verifies, then
   opens the host. Only successful checks are cached, under the existing
   configuration identity, and a proof requires the hosted tools to be
   present as well as unauthorised ones absent.
2. Environment. Inheritance and caller additions are deliberately different
   predicates and stay separate: Grok inherits many `GROK_*` controls while a
   caller may add none of `GROK_*` or `XAI_*`, and additions are also refused
   for loader, proxy and process-identity overrides. The order is kept:
   sandbox inheritance filter, engine strip, cross-engine strip, the Claude
   auto-memory and Grok telemetry overrides, the selected home, then
   validated additions.
3. Server requests fail closed. The dialect recognises its frames and
   encodes refusals; shared policy denies every operation the engine does
   not name. ACP permission answering keeps Grok's once-only selection and
   Command Code's stricter refusal of questions, ambiguous options and
   `switch_mode`, even under an allow policy.
4. Stable public surface and references. `harness.Engine` values, every
   exported error's `Engine` field, `Policy`'s JSON shape, and the normalised
   values that `Ref` digests hash are unchanged. Codex and Claude both receive
   Codex and Claude defaults today; that cross-engine defaulting, foreign-field
   validation before defaults, and nil-versus-empty `ClaudeTools` are kept.
5. Release order: close, reap, reclaim under the lease, write back the login,
   clear the marker. Never write back while the harness still writes, or touch
   an assignment another session acquired. Login also syncs at turn
   boundaries: before Codex `turn/start` and before a terminal finish.
6. Startup checks: durable process identity before initialization; Codex
   sandbox and browser read-back during initialization; Claude's init surface
   check and Command Code's mode check before the active-turn gate. Session
   driver state exists before the wire reader starts, because callbacks can
   arrive before `open` returns.
7. Tool admission: terminal completion closes admission before `done` is
   exposed; a replacement turn waits for handler settlement; Compact never
   reopens tools.
8. Failures and data: uncertain controls close the session, definitive
   rejections keep it; payload bounds, channel-secret scrubbing and
   structural errors without provider prose are unchanged. Transcripts and
   wire bytes are unchanged.

Logic that is genuinely cross-engine with one engine's carve-out stays shared
and consults the engine: skill delivery (engine × sandbox × restriction),
`judgeSurfaces`, Claude's start-up tool-surface check.

## Stages

Each stage is behaviour-preserving, lands on its own, and passes vet (native,
Linux, Windows), the strict race suite and sandboxcheck on macOS and Linux,
and CI before the next begins. Where a later stage owns something an earlier
one touches, the earlier stage forwards through a named temporary accessor.

1. Registry and wire dialect. Resolve envelope, reply parser and
   server-request answerer once per wire; Grok and Command Code permission
   policy move onto the dialect value. Tests first: exact emitted bytes for
   envelopes and every server refusal, unknown requests through the shared
   dispatcher, two simultaneous wires with opposite permission policies.
2. Descriptors and normalisation, preserving historical defaults. Tests
   first: `Policy` JSON goldens and cross-engine normalised-option snapshots
   beside the existing persisted-`Ref` goldens, and an inheritance-versus-
   addition environment matrix that pins the asymmetry.
3. Session state, initialization, startup checks and session notifications
   (Grok model and capacity, Command Code model and mode watch). Tests:
   concurrent same-engine sessions stay isolated; a mutable registry
   singleton is caught.
4. Turn state and lifecycle: start, interrupt, steer, events, streamed
   telemetry, Compact, terminal login sync (Codex totals and compaction,
   Grok response, Command Code message, Claude refusal). Tests: the new
   dispatch route for immediate start-up notifications and late ACP replies;
   fresh state across sequential, refused and compaction turns; the existing
   buffering, steer and settlement tests.
5. Telemetry queries: account, quota, context.
6. Launch preparation, proofs, runtime login preparation and Release. Tests
   first: recorded operation-order tests with failures injected at proof,
   identity recording, reclamation and write-back; write-back failure keeps
   the marker; host and hostless Release paths.
7. Public descriptor (`harness.Describe`) and adoption by `catalog`,
   `completion` and `native`, with completeness and unknown-engine tests.

## What adding an engine looks like afterwards

The engine's constant in `harness.go` and its capability rows (both public
claims, deliberately explicit); the engine's files implementing the contracts
it offers; its registry entry; its tests. A contract it does not implement is
refused with a named reason, never a silent fallback to another engine's
behaviour, and the registry test fails on a missing function.

## Not doing

- A generic ACP engine: the agents differ in exactly the ways that matter
  (resume semantics, instructions, permission questions), so each stays named.
- Changing `Policy`'s shape: it would break every stored reference.
- Moving the API engine onto the CLI adapter: its loop is a different model.
