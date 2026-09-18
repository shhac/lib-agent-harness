# lib-agent-harness

Shared local Codex/Claude CLI harness transport for Go applications.

- Mechanism lives here: process containment, native protocol, model discovery,
  constrained completion, session control, and typed events. Domain prompts,
  scheduling, retries, authorizations, and persistence policy stay in callers.
- Keep constrained application-controlled completion distinct from native coding
  sessions. Restriction is opt-in; preserve ordinary callers' behavior and resume
  references. Never silently widen permissions.
- Restricted sessions prove removal of native tools before a credentialed launch.
  A working directory or write sandbox does not contain reads. Refuse an
  unsupported runtime with an actionable capability error before starting work.
- Caller-hosted tools execute through the caller. Serialize execution; latch a
  closing tool only after the handler succeeds. A refused finish must not close
  the channel. Turn completion pauses admission immediately. CancelTools also
  cancels admitted work; await actual settlement before authorizing another turn.
- Reuse native subscription logins. Restricted Codex uses a private RuntimeHome
  sharing the selected Home's file-backed login without inheriting its config.
  Keep credentials outside workspaces, prompts, logs, and tool results. Owner
  login changes and logout win over worker refreshes. Claude uses the selected
  native login with its restricted mode. Paths and environment are per-client;
  never mutate the parent's environment.
- Capabilities distinguish native, composed, unsupported, and unknown support.
  Interrupted operations never imply rollback. Unknown usage is not free.
  Distinguish response observations from settled turn accounting; health and
  activity are not evidence that an assignment succeeded.
- A harness can outlive its launcher. Contain its process tree; a free bridge
  lock proves only that the bridge is gone. Establish live process identity
  before signalling, and preserve uncertain ownership rather than double-start.
- Verification has no bypass. Cache only evidence for the same binary and launch
  configuration. Probe with disposable homes, dummy credentials, and a local
  provider that refuses inference. Codex overrides use TOML, not JSON; verify
  arguments and protocol shapes against the installed CLI, not fixture echoes.
- Verify both absence of unauthorized tools and positive availability of hosted
  tools. Deferred MCP tools may require channel evidence rather than appearing
  in a model request. Do not relax unauthorized-surface checks to satisfy this.
- Keep provider protocol parsers and typed failure facts here. Error values and
  structural facts exclude raw provider text; diagnostic callbacks are bounded
  and sanitized. Callers own retry decisions after possible side effects.
- Tests use fake transports, synthetic CLI fixtures, and temporary directories.
  No real models, accounts, credentials, or external mutations in automated tests.
- Run go test -race ./... and go vet ./...; cross-compile Windows tests too.
  Restricted hosting is unavailable on Windows; test the pre-launch refusal and
  retain ordinary session coverage there.
- Use git-hunk for staging. Only the primary agent commits/pushes, on main.
- Dependencies use published versions, never committed local replace directives.
