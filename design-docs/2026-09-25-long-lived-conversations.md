# Long-lived conversations

Dated 2026-09-25. Pinned against Claude Code 2.1.282 and codex-cli 0.156.1 on
macOS (Darwin 25.6). Shipped in v0.4.0.

## Why

A caller (crew-assistant) is moving its assistant chat onto one long-lived
restricted session per conversation, for both engines. The session stays open
across turns while the daemon runs and is resumed from a saved `Ref` after a
restart. When resuming can't work, the caller rebuilds from its own record.

That needs three things any caller of a long-lived session needs:

- a way to know whether it got its conversation back;
- stored conversations that survive a release that edits its tools;
- a way to refresh the caller's context after a compaction, without the harness
  replaying anything.

## Open

`Open(ctx, o, ref) (*Session, Opened, error)` resumes when it can and otherwise
starts, and `Opened{Resumed, Fresh}` says which.

`Fresh` is one of two reasons:

- `FreshIncompatible`: the reference names another engine or configuration.
- `FreshUnavailable`: the harness no longer has the conversation.

Every other failure is returned with no session, because the caller has to hold
rather than rebuild. That covers transport, capability, `ErrUnreclaimed` and
`ErrLeaseHeld`.

**Reclaim first, under the lease.** A restricted session's directory is
reclaimed before anything is launched, and again after a resume that failed
because the conversation was gone. The spec said "Reclaim first". Doing that
without the lease would let `Open` identify and kill a harness that another
live session is driving right now, since a live bridge names its launch either
way. So `Open` takes the assignment lease, reclaims under it, and releases it
before launching. A live session's lease makes `Open` return `ErrLeaseHeld`
untouched. A confirmed reclaim also clears the launch record, so a later reclaim
never judges a group identifier the system has since reused.

**What "gone" means, per engine:**

- **Claude.** The native transcript must be there before `--resume` is tried.
  The rule comes from the CLI's bundled code and was confirmed by a run with a
  disposable home and a provider that refused inference:
  - The path is `<config>/projects/<key>/<session id>.jsonl`.
  - `<config>` is `CLAUDE_CONFIG_DIR`, or `~/.claude`. The harness passes
    `Options.Home` as `CLAUDE_CONFIG_DIR`, and leaves it unset only when it is
    `~/.claude`, so it is always `Options.Home`.
  - `<key>` comes from `realpath(cwd)`. Every UTF-16 unit that is not `[A-Za-z0-9]`
    becomes `-`. A key longer than 200 is cut to 200 and suffixed with `-` and
    `base36(abs(h))`, where `h` is the Java-style 32-bit string hash. The
    long-path vector in the tests came from the real CLI.
  - `CLAUDE_CODE_PROJECT_DIR_NAME`, when `CLAUDE_CONFIG_DIR` is set and the name
    is valid, replaces the key. The harness does not strip it from the parent's
    environment, so `Open` honours it too.
  - A resume that misses the computed path scans every project folder and
    accepts a single match. `Open` does the same.
- **Claude, the fallback.** A transcript can exist and still not be reopenable.
  Resuming a missing conversation in 2.1.282 writes an error `result` frame and
  exits with status 1. It sends no init frame and no reply to `initialize`. So
  a resumed Claude whose process exits naturally during startup is also treated
  as gone. A harness the library had to kill is not evidence either way.
- **Codex.** 0.156.1 answers `thread/resume` for an unknown thread with
  `-32600 "no rollout found for thread id …"`, which the transport maps to
  `ErrRejected`. Only that request's rejection counts.

Both are marked internally with an unexported sentinel. The errors `Resume`
returns still wrap the originals (`*ProcessError`, `ErrRejected`), so existing
`errors.Is`/`errors.As` checks keep working.

## The resume hash

A restricted session's `Ref.ConfigHash` covers:

- the legacy fields (binary, model, effort, instructions, policy);
- that the session is restricted;
- the tool server's name and the runtime home.

It no longer covers the tool definitions. The reference never vouched for the
restriction: `prepareLaunch` re-proves it on every Start and Resume, and Claude's
init frame is cross-checked against the tools configured for that launch. A
transcript that mentions a tool which has since changed is harmless.

Instructions stay in the hash, persona included. A persona change starts a
fresh, seeded conversation. That's rare, and it's the correct outcome.

The digest's shape changed, so references stored for restricted sessions by
earlier versions stop matching once. `Open` reports that as `FreshIncompatible`.

## Caller context

`Options.Context func(ctx, ContextReason) (string, error)` is asked at
`StartTurn` when a reason is pending:

- `ContextStarted`: set when a session is started rather than resumed.
- `ContextCompacted`: set when either engine reports a completed compaction:
  - Claude: `system/compact_boundary`, auto or manual.
  - Codex: an `item/completed` whose item is a `contextCompaction`, including one
    inside a `Compact` turn, plus the deprecated `thread/compacted`.

A compaction replaces a pending "started". It can only happen inside an accepted
turn, and that turn carried whatever was pending when it began.

**How it's delivered.** The text goes ahead of the input as
`<caller-context reason="…">\n` + text + `\n</caller-context>\n\n` + input.
Empty text sends the input unchanged.

**When it clears.** The handler runs under the operation gate. An error fails
`StartTurn` and keeps the reason. The reason clears once the harness has accepted
the turn, and only if no newer mark arrived meanwhile. That second condition
matters: a generation counter stops a compaction reported while the turn was
starting from being cleared by that same turn.

**How it's kept.** A restricted session writes the reason to
`<Tools.Dir>/context.pending` (0600, beside the launch record) and reads it back
on Resume, so a daemon restart doesn't lose a compaction. An ordinary session
keeps it in memory. A marker that can't be written still holds in memory, and
is reported through `OnDiagnostic` as `context_marker_unwritten`.

## Manual compaction on Claude stays unsupported

Restricted Claude launches with `--disable-slash-commands`, which removes
`/compact` along with every other command. Lifting it would need a security
review of the whole command surface, so `Capabilities().Compact` stays
unsupported for Claude, which compacts automatically. A caller that wants a
manual compaction rebuilds instead:

1. Summarize the conversation itself.
2. `Open` with no reference.
3. Supply the summary as the `ContextStarted` context.

## Also fixed

Claude's compaction trigger is read from `compact_metadata.trigger`, where
2.1.282's SDK frame puts it. The previous flat spellings never matched, so every
real compaction was reported as `unknown`.

## Not built

- `Options.AutoCompact` (dropped in review).
- Replacing instructions in restricted sessions.
- A private Claude config home for chat transcripts (the login lives in the
  keychain).
- Codex `thread/inject_items`.
