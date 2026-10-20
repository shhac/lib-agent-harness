# fx support (unreleased)

Adds `harness.Fx` (`"fx"`), a CLI engine for fx, the terminal coding agent
whose CLI is installed as `fx`. Every shape was checked against fx 0.0.14.

fx is offered in all three execution modes it can carry:

- **Sessions** run `fx acp` over the Agent Client Protocol. A session starts,
  resumes, interrupts and steers (composed), with streamed text, tool started
  and completed events, the turn's own usage with its cache reads and writes,
  and the context window fx itself states.
- **Native runs** are one `fx ask --json`. fx states no output schema, so a
  structured request is refused. An asking run, the default, fails with
  `NonInteractivePermissionRequired` at its first write or shell command.
- **Model discovery** reads `fx models --json`, and **account inspection**
  reads `fx status` and `fx credits`, in a temporary directory, with no
  inference and no conversation.

What fx cannot carry is refused rather than approximated:

- The mode, the model and the effort are set as the session's own
  configuration and read back from what fx reports. fx's `session/set_mode`
  answers with nothing, so it is not used: a mode that cannot be read back is
  a permission the library will not run with. A model or effort fx would
  substitute is refused before the first prompt.
- Every session runs in fx's asking mode, so fx asks before every action that
  changes anything. New `Policy.FxPermission` answers those requests:
  `FxDenyWhenAsked` (the default) or `FxAllowWhenAsked`, never an "always"
  option. fx's own tools stay available, so an fx session is not a read-only
  mode: fx 0.0.14 asks before writes and shell commands, but reads files and
  fetches web pages without asking.
- A permission request that offers two options of the same kind or the same id
  is answered cancelled, so a deny can never be read as an allow.
- fx appends session instructions to its own system prompt and offers no way
  to replace it, so a replace request is refused.
- fx keeps its profile in the user's home directory and has no variable that
  moves it, so a home that names another directory is refused. A session
  inherits none of fx's own `FX_` variables and refuses them as additions,
  except the operator's own protective switches `FX_MAX_AGENT_STEPS`,
  `FX_ULTRAFAST` and `FX_AUTO_UPGRADE`, whose bounds, cost and network
  choices are preserved. fx has no environment variable that carries a privacy
  guarantee: its Zero Data Retention control is a Gateway account setting.
- `Complete` is unsupported: fx offers no way to prove its native tools are
  absent. Restricted, sandboxed, browser and compacted sessions, hosted caller
  tools, and fx quota are unsupported too.

New native failure code `CodeHomeUnsupported`. `Policy` gains a field, so
unkeyed (positional) `Policy` literals no longer compile; keyed literals are
unaffected. Existing engines, references and digests are unchanged.

A session's `Resume` reports a conversation fx no longer holds as
`errConversationGone`, and a permission request that offers no option the
session's policy wants is answered cancelled, so no permission and no answer
is ever guessed.
