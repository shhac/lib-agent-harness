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
- A harness outlives the process that launched it. Contain it in its own group,
  anchor liveness on the bridge lock, and confirm termination before calling a
  run recoverable. Provider text and captured output stay out of error values.
  Publish usage as it is observed and mark whether it is one response or a
  turn's accounting. A failed turn's observations are evidence, never a
  measurement. Health describes a process; it never means work finished.
- A harness outlives the process that launched it. Contain it in its own group,
  anchor liveness on the bridge lock, and confirm termination before calling a
  run recoverable. Provider text and captured output stay out of error values.
- Tests use fake transports, synthetic CLI fixtures, and temporary directories.
  No real models, accounts, credentials, or external mutations in automated tests.
- Run go test -race ./... and go vet ./...; cross-compile Windows packages.
- Use git-hunk for staging. Only the primary agent commits/pushes, on main.
- Dependencies use published versions, never committed local replace directives.
