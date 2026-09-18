# lib-agent-harness

Shared local Codex/Claude CLI harness transport for Go applications.

- Mechanism lives here: process containment, native protocol, model discovery,
  constrained completion, session control, and typed events. Domain prompts,
  scheduling, retries, authorizations, and persistence policy stay in callers.
- Preserve the distinction between constrained application-controlled completion
  and native tool-enabled agent sessions. Never silently widen permissions.
- A restricted session removes the harness's own tools and proves it against the
  installed CLI before a credentialed process starts. Restricting writes is not
  restricting reads, and a working directory is not a filesystem boundary:
  neither may be presented as containment. Containment that cannot be
  established fails closed with an actionable capability error; never launch.
- Restriction is opt-in per session. Do not change what an unrestricted session
  does on behalf of a caller that did not ask for it.
- Caller-hosted tools are the caller's to execute. The library serves the
  protocol, owns the channel and its lifetime, and touches nothing itself. A
  closing tool latches the channel on admission, not on return.
- A restricted session removes the harness's own tools and proves it against the
  installed CLI before a credentialed process starts. Restricting writes is not
  restricting reads, and a working directory is not a filesystem boundary:
  neither may be presented as containment. Containment that cannot be
  established fails closed with an actionable capability error; never launch.
- Restriction is opt-in per session. Do not change what an unrestricted session
  does on behalf of a caller that did not ask for it.
- Caller-hosted tools are the caller's to execute. The library serves the
  protocol, owns the channel and its lifetime, and touches nothing itself. A
  closing tool latches the channel on admission, not on return.
- Reuse native CLI login storage; never copy or expose credentials. Binary/home
  paths are per-client configuration, never process-global environment mutations.
- Capabilities distinguish native, composed, unsupported, and unknown support.
  Interrupted operations never imply rollback. Unknown usage is not free.
  Publish usage as it is observed and mark whether it is one response or a
  turn's accounting. A failed turn's observations are evidence, never a
  measurement. Health describes a process; it never means work finished.
- A harness outlives the process that launched it. Contain it in its own group.
  Absence is established from the process group; a free bridge lock proves only
  that no bridge is running. Identity is established from something alive, never
  from a stored identifier, and nothing is signalled without it. Unconfirmed is
  reserved work, not permission to start a second one. Provider text and
  captured output stay out of error values.
- Verification has no bypass. Skipping a repeat requires a record of the same
  binary and arguments, never a caller's assertion. Probe with the arguments the
  real launch will use, or the check is about a different configuration.
- Provider configuration must be encoded in the form that provider parses: Codex
  overrides are TOML, not JSON. A fixture that echoes a flag back cannot prove an
  installed CLI accepts it. Check flags, names and request shapes against the
  installed binary with a disposable home and a provider that rejects everything,
  and encode what was measured — not what the documentation implies.
- Restriction is judged on two separate questions: nothing unauthorized in any
  request, and the hosted surface positively proven. A harness that defers its
  MCP tools will never show them in a request; the tool channel's own record is
  the evidence there. Do not relax the first question to satisfy the second.
  Publish usage as it is observed and mark whether it is one response or a
  turn's accounting. A failed turn's observations are evidence, never a
  measurement. Health describes a process; it never means work finished.
- A harness outlives the process that launched it. Contain it in its own group.
  Absence is established from the process group; a free bridge lock proves only
  that no bridge is running. Identity is established from something alive, never
  from a stored identifier, and nothing is signalled without it. Unconfirmed is
  reserved work, not permission to start a second one. Provider text and
  captured output stay out of error values.
- Verification has no bypass. Skipping a repeat requires a record of the same
  binary and arguments, never a caller's assertion. Probe with the arguments the
  real launch will use, or the check is about a different configuration.
- Provider configuration must be encoded in the form that provider parses: Codex
  overrides are TOML, not JSON. A fixture that echoes a flag back cannot prove an
  installed CLI accepts it. Check flags, names and request shapes against the
  installed binary with a disposable home and a provider that rejects everything,
  and encode what was measured — not what the documentation implies.
- Restriction is judged on two separate questions: nothing unauthorized in any
  request, and the hosted surface positively proven. A harness that defers its
  MCP tools will never show them in a request; the tool channel's own record is
  the evidence there. Do not relax the first question to satisfy the second.
- Tests use fake transports, synthetic CLI fixtures, and temporary directories.
  No real models, accounts, credentials, or external mutations in automated tests.
- Run go test -race ./... and go vet ./...; cross-compile Windows packages.
- Use git-hunk for staging. Only the primary agent commits/pushes, on main.
- Dependencies use published versions, never committed local replace directives.
