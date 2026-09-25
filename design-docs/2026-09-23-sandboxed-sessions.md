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

## Established with real inference (same day, before v0.3.1)

A smoke run asked each engine, in both modes, to try six things: create a
file in the workspace with its file tool; create one outside the workspace
with its file tool; write outside with the shell; write `/tmp` with the
shell; run `curl https://example.com`; run `git ls-remote` against GitHub.

- **Claude Code, writing:**
  - the file in the workspace was created;
  - the Write tool was refused outside the workspace by permission rules;
  - both shell writes failed with "operation not permitted";
  - `curl` and `git` failed at the proxy with 403.
- **Claude Code, read-only:** no file tools, the shell writes were denied, and
  the network was blocked.
- **Codex, writing:**
  - `apply_patch` created the workspace file and was rejected outside it;
  - the shell writes were denied;
  - DNS was blocked for both network commands.
- **Codex, read-only:** everything was rejected.
- **v0.3.0 bug:** the first Codex run showed that v0.3.0 had also disabled the
  `code_mode_host` feature. Every tool call then failed with "code-mode host is
  disabled", so nothing escaped, but nothing could be done either. v0.3.1 keeps
  it enabled, with a test.
- **Resume readback:** a Codex thread that had completed one turn was resumed
  under the sandbox, and thread/resume reported the profile. The readback
  passed before any prompt.

Nothing listed as unestablished below remains open.

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

## v0.3.2 and v0.3.3, same day

- **Correction.** The "personal instructions" finding that motivated
  `claudeMdExcludes` was wrong. A diagnostic turn with a canary instruction
  above the workspace and another inside it showed that, with
  `--setting-sources=` empty, Claude Code 2.1.280 loads no instruction files at
  all, including the workspace's own. The owner's name the role used came from
  the account email in its context, not from their files. The exclusions stay
  as a second guard, and the README now says repository instructions have to
  be pointed to.
- **Sandboxed sessions inherit an allowlisted environment.** `Options.Env`
  refuses variables that change the unsandboxed CLI itself (loader, Node,
  proxy, certificate, `GIT_*`).
- **Claude:** auto-memory and skills are disabled; its `.git` is read-only
  (v0.3.2); the Codex canary now also proves `.git` is read-only.
- **Verified with real turns under the allowlist:** Claude's `.git` writes
  were refused by shell and tool alike, and Codex ran `go test` in its sandbox
  with its cache and `TMPDIR` in the workspace.

## Web access and hosted tools, 2026-09-26

Checked against Claude Code 2.1.282 and codex-cli 0.156.1 on macOS, with
disposable homes, dummy keys and a loopback provider that refused every
request. Nothing below used inference.

- **Claude's WebFetch domain rules reach the shell.** The 2.1.282 bundle
  builds the sandbox's network allowlist from `sandbox.network.allowedDomains`
  and from the `domain:` content of every `WebFetch(domain:...)` allow rule;
  a bare `WebFetch` rule has no content and adds nothing. So `Sandbox.Web`
  allows the bare tool names only. `dontAsk` needs those rules: without an
  allow rule a web tool is refused.
- **Claude with Web** advertised `WebFetch` and `WebSearch` in its init frame
  and in the request's `tools`, beside the sandboxed native set.
- **Codex's `web_search="live"`** accepts `disabled`, `cached`, `indexed` and
  `live`. With a provider that declares `supports_standalone_web_search`, it
  adds a `web__run` tool (search, open, find) inside the code-mode `exec`
  surface. `disabled` removes it. The loopback provider does not declare
  support, so its requests carried no web tool in either mode; the built-in
  OpenAI provider is expected to. This was not confirmed with a real login.
- **Hosted tools, Claude:** with `--strict-mcp-config`, `--mcp-config` naming
  the bridge, and no `mcp__*` deny, the init frame reported the server
  `connected` and `mcp__<server>__<tool>` beside the native tools, and the
  request carried it. The tool channel served its list. Claude Code applies
  deny rules before allow rules, so the old `mcp__*` deny would have refused
  the hosted tools.
- **Hosted tools, Codex:** the bridge, launched by the app-server as an MCP
  server, reached the channel from a sandboxed session and was served its
  tools. The tools were deferred as before. Under the same profile,
  `codex sandbox` running a Unix-socket client was refused with EPERM, so the
  shell cannot reach the channel even though it can read `ToolHost.Dir`.
