# Sandboxed native sessions

Dated 2026-09-23. Pinned against codex-cli 0.154.0 and Claude Code 2.1.280 on
macOS (Darwin 25.6).

## Why

A caller (crew-assistant) needed ordinary native coding sessions with their own
tools, held to workspace-only writes and no network, that fail closed when
that cannot be shown. Restricted sessions answer a different question: they
remove the tools. This added a third mode: ordinary tools, OS sandbox.

## What was established without inference

- **Codex's built-in `:workspace` profile was not enough.** It allowed writes
  to `/tmp`, `/private/tmp` and `$TMPDIR`, and silently ignored the
  `sandbox_workspace_write.*` overrides.
- **A custom profile passed as `-c` overrides worked.** Filesystem `:root`
  read, `:workspace_roots` `.` write or read, `.git` read, network disabled:
  - under `codex sandbox -P`, the canary's out-of-workspace and temp-directory
    writes failed, and so did loopback connections;
  - under `app-server`, thread/start reported
    `activePermissionProfile: {id: harness_sandbox}` with `workspaceWrite`,
    `networkAccess: false` and both temp excludes true (or `readOnly`).
- **The legacy mode displaced the profile.** Passing `sandbox:
  "workspace-write"` in thread/start dropped the profile
  (`activePermissionProfile: null`) and re-allowed the temp directories. So the
  library omits the mode for sandboxed sessions and checks the response.
- **`activePermissionProfile` needs the experimental API.** It is reported only
  when initialize advertises `experimentalApi`, and the library does so only
  for sandboxed sessions.
- **Claude emits nothing before its first user message,** so there is no
  pre-inference readback. `claude sandbox status` with the session's own
  settings reported `supported`, `enabled` and `strictMode` true and
  `unavailableReason` null. It also reported `available` and `installed`
  false, which describe an optional Windows installer, so the check ignores
  them.
- **Negative check:** flipping the profile's network to enabled made the real
  canary fail with `sandbox_not_enforced`.
- **Review correction before release.** An independent review found that the
  first canary's temp-directory checks were vacuous: they wrote to a path that
  was the probe's own directory, which fails with or without a sandbox. A
  read-only canary could also pass without having run. The canary now writes
  distinct files and must print a completion marker. A test runs the script
  with no sandbox and requires every escape to be reported. The corrected
  canary passed against the real CLI in both write modes.
- **The Claude status check** asks from a throwaway home and gave the same
  answer.
- **The Codex profile also covered:**
  - `.codex` and `.agents` read-only;
  - connectors, plugins, hooks, browser use, computer use, multi-agent and
    dependency installs disabled.

  With these settings, thread/start still reported the profile.

## Not established

- That thread/resume reports `activePermissionProfile` the way thread/start
  does. If it does not, sandboxed resumes fail closed. This could not be
  checked without inference: resuming a thread that had never been prompted
  was rejected by the installed app-server, which appears to persist a thread
  only after its first turn.

- That a real Codex turn's `apply_patch` obeys the profile's writable roots.
- That a real Claude turn's Bash receives exactly the checked policy.
- That the Claude file tools stay inside `WorkDir` under `dontAsk` with the
  Edit allow rule.

A caller's first real run should attempt each deliberately.
