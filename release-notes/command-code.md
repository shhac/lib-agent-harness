# Command Code sessions (unreleased)

Adds `harness.CommandCode` (`"command-code"`), a CLI engine for
Command Code, whose CLI is installed as `cmd`. It is
offered only as a session, over `cmd acp` (the Agent Client Protocol), and
checked against Command Code 1.74.1. `Complete`, `Run`, `Models` and `Account`
are unsupported with reasons, as is a session on Windows.

- Sessions start, resume, interrupt and steer (composed), with streamed text,
  tool started and completed events, the turn's own usage and Command Code's
  context figure.
- Model and effort are set with `session/set_config_option` and refused before
  the first prompt unless the session reports them.
- Every session runs in Command Code's Standard mode, which asks before each
  change. New `Policy.CommandCodePermission` answers: `CommandCodeDenyWhenAsked`
  (the default) or `CommandCodeAllowWhenAsked`, never an "always" option.
- Command Code resumes an unknown id as an empty conversation, so a resume
  checks `session/list` first. `Open` starts fresh with `FreshUnavailable`.
- New capability code `CapabilityChangedPermissionMode`.

Instructions, provided skills, `GlobalSkillsExclude`, restricted, sandboxed,
browser and compacted sessions are refused. Existing engines, references and
digests are unchanged.
