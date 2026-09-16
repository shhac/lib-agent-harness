# lib-agent-harness

Shared local Codex/Claude CLI harness transport for Go applications.

- Mechanism lives here: process containment, native protocol, model discovery,
  constrained completion, session control, and typed events. Domain prompts,
  scheduling, retries, authorizations, and persistence policy stay in callers.
- Preserve the distinction between constrained application-controlled completion
  and native tool-enabled agent sessions. Never silently widen permissions.
- Reuse native CLI login storage; never copy or expose credentials. Binary/home
  paths are per-client configuration, never process-global environment mutations.
- Capabilities distinguish native, composed, unsupported, and unknown support.
  Interrupted operations never imply rollback. Unknown usage is not free.
- Tests use fake transports, synthetic CLI fixtures, and temporary directories.
  No real models, accounts, credentials, or external mutations in automated tests.
- Run go test -race ./... and go vet ./...; cross-compile Windows packages.
- Use git-hunk for staging. Only the primary agent commits/pushes, on main.
- Dependencies use published versions, never committed local replace directives.
