# The API workbench: workspace tools for OpenAI-compatible sessions

Proposed 2026-09-29, written 2026-09-30. Revised the same day:

- how the workbench tools reach the model as well as the host;
- OpenRouter's embedded errors, reconciled with main's `EmbeddedFailure` and
  streaming.

Revised again the same day, after a second review:

- the hard-link rule applies from stage 1, on the open handle;
- the bwrap mount layout under a hidden home;
- crash handling for atomic replacement.

And after a third review: mount points below `WorkDir`, and opens that never
block on a FIFO or device.

And after a fourth review:

- `WorkDir` and `RuntimeHome` kept disjoint, and `RuntimeHome` hidden from
  commands;
- a symlink-free write path;
- a tracked, bounded workspace worker, so that cancellation waits for real
  settlement;
- a deterministic final file mode.

Tests now skip through main's `internal/testenv`.

And after a fifth review:

- command reads are an allowlist (a pinned system set, `WorkDir`,
  `Commands.Read` and the scratch) on both platforms;
- Linux starts from an empty root, with its mounts in five fixed phases, so no
  tmpfs is laid over home or `RuntimeHome`, and the canary runs with
  `RuntimeHome` inside and outside `$HOME`;
- failures after the rename are unknown outcomes, never rollbacks.

And after a sixth review:

- Linux phase 2 now creates every directory, system-set destinations
  included, before anything is bound;
- bwrap 0.8.0 is pinned, with `--unshare-user --disable-userns --cap-drop
  ALL`;
- the canary and an adversarial CI test try to unmount, remount and rebind
  around the `.git` overlay, and every attempt must fail.

**Status.** Stage 1 part 1 (LAH-2) is implemented but switched off. It adds
`Options.Workbench`, the `WorkDir` and `RuntimeHome` rules, the reserved names,
the `Ref` digest, and `read_file` and `list_files` wired to the model and the
host. Every session that sets `Workbench` is still refused with
`RefusedNotOffered`, so no caller can reach the tools, and `Support` claims
nothing. Stage 1 part 2 switches it on. It adds the opened-handle checks,
non-blocking opens, the workspace worker, `search_files`, `Support`, the
README and CI (see "Stage 1 lands in two parts" below). Stage 2 (LAH-3) is not
implemented. The work is staged in the tasks at the end.

This supersedes one line in two earlier documents: "sandboxing is never
claimed" for API sessions, in
[the API transports design](2026-09-27-api-transports.md) and in increment 6 of
[the unified-harness design](2026-09-27-unified-harness.md). That line is
true today, and it stays true until stage 2 lands. From stage 2, the command
tool the library itself provides runs inside a sandbox that the library proves
before launch, and `Support` claims that sandbox as `unknown`.

## Why

A composed API session ([increment 6](2026-09-27-unified-harness.md)) has only
the caller's hosted tools and composed skills. That is enough for a model to
talk. It is not enough for a model to be a team member in crew-assistant,
because every role there works in a repository:

- a **researcher, reviewer or PM** has to read, list and search files;
- an **implementer or QA** also has to edit files and run the build and the
  tests.

The CLIs bring these tools with them, and the library proves their sandboxes.
An endpoint brings nothing. Each application could write its own file tools
and command runner, but then every application would redo the confinement and
the sandbox proof, which is exactly the mechanism `AGENTS.md` keeps in this
library. So the library provides the tools itself. They are called the
**workbench**.

The main target is inexpensive and free models behind OpenRouter. The section
on OpenRouter below covers what that gateway needs beyond the existing
transport.

## Shape of the API

```go
session.Options{
    Provider:    harness.Provider{Engine: harness.OpenAICompatible, API: api},
    Model:       "vendor/model",
    RuntimeHome: "/private/state/api-sessions",
    WorkDir:     "/work/repo",          // required with a workbench; refused without one
    Restriction: &session.Restriction{Tools: host}, // still required: roles keep their closing tool
    Workbench: &session.Workbench{
        Write: true,                    // stage 2a: write_file and edit_file
        NewFileMode: 0o644,             // a new file's mode; default 0600, never the umask
        Commands: &session.Commands{    // stage 2b/2c: run_command, sandboxed
            Loopback: false,
            Read:     []string{"/opt/shared/gomodcache"},
            Env:      []string{"GOFLAGS=-mod=mod"},
            Timeout:  2 * time.Minute,
        },
    },
}
```

- `Workbench` is accepted only for `harness.OpenAICompatible` sessions. For a
  CLI engine it is refused with `RefusedNotOffered`, because the CLI's own tools
  and `Sandbox` already cover this. The CLIs' `Sandbox` struct is not reused.
  Its fields describe a harness's native tools, and its semantics do not change.
- **`WorkDir`.** `normalizeAPI` refuses it today. With a workbench it is
  required. It must be an existing absolute directory, and it is resolved
  through symlinks the way `normalizeSandbox` resolves it, because the macOS
  profile and the bwrap binds name the real path. Without a workbench it is
  still refused, as it is today.
- **`WorkDir` and `RuntimeHome` are disjoint.** `RuntimeHome` holds every API
  session's transcript and every workbench's scratch. A workspace that
  contains it would let the read tools send other sessions' conversations and
  state to the provider. A workspace inside it would sit among them. So both
  are resolved with `EvalSymlinks`, and the session is refused
  (`RefusedWorkDir`, "the workspace and the runtime home must not contain one
  another") when either one is, or contains, the other. The comparison is by
  path, element by element, and is case-insensitive where the file system is:
  on macOS and Windows both names are also compared with `os.SameFile` at each
  shared ancestor. `normalizeRuntimeHome` today resolves only with `Abs`, so
  the workbench's check resolves `RuntimeHome` itself rather than trusting the
  normalized value. Tests:
  - `RuntimeHome` inside `WorkDir`, `WorkDir` inside `RuntimeHome`, and the two
    equal, each refused;
  - one of them reached through a symlink, and one named in a different case
    on a case-insensitive volume, each refused;
  - siblings, accepted.

  A tree aliased by a mount below `WorkDir` cannot reopen `RuntimeHome` either,
  because the mount rule below refuses to cross it.
- **`Restriction.Tools` stays mandatory.** Every role keeps its closing tool.
  The closing-tool rule is unchanged: no workbench tool is a closing tool.
- **Reserved names.** `read_file`, `list_files`, `search_files`, `write_file`,
  `edit_file` and `run_command` are reserved whenever a workbench is set, even
  for the tools that stage is not offering. This means a later stage cannot
  collide with a caller's tool. A caller tool with one of these names is refused
  with a new `RefusedWorkbenchToolReserved`, the counterpart of
  `RefusedSkillToolReserved`. The refusal is necessary, not just tidy:
  `newDirectToolHost` keys its tools by name, so a colliding caller tool would
  otherwise be silently replaced by the library's.
- **`Commands` does not imply `Write`.** When `Write` is false, commands see
  `WorkDir` as read-only, and only the private scratch directories below are
  writable. A reviewer can then run the tests without being able to change the
  code. This follows the semantics of `Sandbox.Write`.
- **`Commands.Env`** adds entries to the allowlisted environment. It uses the
  same refusals as `Options.Env` for sandboxed sessions: no loader, proxy,
  certificate or `GIT_*` variables, and none of the variables the library
  manages (`HOME`, `TMPDIR`, `PATH`, the process marker).
- **`Commands.Read`** is validated by `sandboxReadDirs`, so it can never reopen
  the home directory. It is also refused (`RefusedSandboxRead`) when any entry
  is, contains, or lies inside `RuntimeHome`, by the same comparison as above.
  An entry inside `WorkDir` is allowed, because it adds nothing.
- **Commands never see `RuntimeHome` either.** Commands read only the read
  set defined in "What commands may read" below. `RuntimeHome` is refused if it
  lies inside any part of that set (`RefusedRuntimeHome`, "the runtime home
  would be readable by commands"), so the only part of it a command can reach
  is this session's own scratch, which is added explicitly. The canary's
  `runtime` check proves it: a stand-in transcript beside the scratch must be
  unreadable.
- **The `Ref` digest** gains the workspace and the workbench only when a
  workbench is set. This follows the pattern `skillsDigest` already uses, so
  every existing digest pinned in `session/ref_golden_test.go` stays the same.
  The payload holds `WorkDir`, `Write`, whether commands are enabled,
  `Loopback` and `Read`, but not `Env` or `Timeout`. When `Write` is set, the
  payload also holds the effective new-file mode: `NewFileMode` as
  `normalizeAPI` resolves it, so an unset mode is recorded as 0600, stored as
  its permission bits as a number. The mode decides who can read the files the
  model creates, so resuming under a wider mode would quietly widen who can
  read them. Without `Write` the mode is left out: no workbench tool creates a
  file, and `Write` is itself a digest input, so switching a session from
  read-only to writing is refused as a mismatch anyway. Leaving it out also
  keeps stage 1 digests stable. A resume must name the same workspace, the
  same powers and, when writing, the same effective mode; any difference is
  refused with `ErrIncompatibleResume` by `compatible` in `open`, before the
  transcript is opened, locked or written. The stored `Ref` and the transcript
  header keep the original `ConfigHash`, and the caller can retry with the
  recorded configuration. A resumer that bypassed `compatible` would still
  fail the transcript header's `ConfigHash` check on load. `Ref.WorkDir` is
  filled in when a workbench is set.
- **Events.** A workbench call is reported as a tool event, the same way a
  composed skill call is. `ToolActivity` holds its arguments and its bounded
  result.

### How the tools reach the model and the host

A composed session has two separate lists of tools, and a workbench tool has
to be in both:

1. **What the model is offered.** `apiTools(o)` builds the `tools` array that
   every Chat Completions request carries. Today it serializes only
   `Restriction.Tools.Tools`. The skill tools reach the model a different way:
   completion adds them itself from `Config.Skills`. Completion knows nothing
   about a workbench, so unless `apiTools` includes the workbench definitions,
   the model never sees them. Worse, `parseChatCompletion` rejects a whole
   response whose tool call names a tool that is not in the supplied catalog,
   so a model that guessed `read_file` would get `invalid_tool_call` for the
   response, not a workspace read.
2. **What the host admits and answers.** `newDirectToolHost(host, extra...)`
   admits calls. A `workbenchHandler` answers the workbench's calls, in the same
   position in front of the caller's handler as `apiSkillHandler`, and passes
   every other call on.

To keep the two from drifting apart, one function, `workbenchDefinitions(o)`,
returns the stage's `ToolDefinition`s: name, description and JSON schema.
Both places use it:

- `apiTools(o)` appends them after the caller's tools, converted the same way
  (`completion.Tool{Type: "function", ...}`);
- `newDirectToolHost` receives them next to `apiSkillDefinitions(o)`.

It returns only the tools the options switch on:

- the read tools for `Workbench{}`;
- `write_file` and `edit_file` as well, with `Write`;
- `run_command` as well, with `Commands`.

A tool the options do not switch on is in neither list, so a call to it fails
as an unknown tool, just as a call to any other unconfigured name does.

The same rule applies on every path that builds these lists:

- **Start, `Open` and `Resume`** all go through `openAPI`, which builds
  `apiSession.tools` and the host once from the normalized options. Adding the
  workbench there covers all three. A resumed session whose options have no
  workbench offers no workbench tools, but its `Ref` digest already differs
  (see below), so it cannot be mistaken for the original.
- **The collision checks** run in `normalizeAPI`, for caller tools against the
  workbench names. The skill names are distinct from the workbench names by
  construction, and a unit test pins that the two sets do not overlap.
- **The request size.** The definitions count towards `Loop.MaxRequestBytes`
  like any other tool definition. They are short and fixed, and a test pins
  their encoded size so that a later edit cannot bloat every request.
- **Result bounds.** A workbench result is bounded by the tool's own limits
  (the table below) *and* by the host's `MaxResultBytes` (default 64 KiB), the
  smaller of the two. The host's truncation note then never cuts a workbench
  result without saying so.

Stage 1's tests must prove both paths:

- **A request-shape test.** The fake endpoint records the request body and
  asserts that `tools` holds the caller's tools *and* exactly the stage's
  workbench tools, with their schemas. Another case asserts that a session
  without a workbench sends exactly today's list. It is repeated for `Write`
  and `Commands` as those stages land.
- **A loop test.** The fake model calls `list_files`, `search_files` and
  `read_file` in turn, then the caller's closing tool. The test checks each
  result the loop appended, and that the caller's handler saw only its own
  call.

Every tool result that goes to the model is text built by the library. Errors
are fixed codes, such as `file_outside_workspace`, `file_not_text`,
`file_too_large`, `file_not_regular`, `file_linked`, `file_other_mount`,
`file_reserved`, `match_not_found`, `match_not_unique` and `command_timeout`,
together with the relative path the model supplied. No absolute host path,
environment value or OS error prose is included.

## Confinement of the file tools

All the file tools are pure Go, run in the library's process and never start a
shell. They run outside any OS sandbox, so their confinement is the library's
own path handling. Two patterns exist in the repository:

- `internal/skills` (`cleanRelative`, `resolve`, `within`, then a `SameFile`
  check on the opened file). This checks and then uses the path. Its own comment
  explains that this is safe for a skill directory the caller owns. It is
  **not** safe for a workspace in which the model can also run commands: a
  background process that one command started could swap a directory for a
  symlink between the check and the open.
- `os.Root` (Go 1.24+, already used by `completion/scratch.go`). It resolves
  every path relative to a directory handle. It follows symlinks only while they
  stay inside the root, and it is safe against these races on macOS, Linux and
  Windows. It confines *names* only. Its own documentation says it does not stop
  traversal into a file system mounted below the root, including Linux bind
  mounts, and it does not look at what a name opens: a hard link, a FIFO or a
  device. The rules below close those gaps.

**The workbench uses `os.Root`**, opened once on `WorkDir` for each session.
From `internal/skills` it keeps two things: `cleanRelative`'s rules for model
input, which are slash-separated, relative, clean, have no `..` and no volume
or NUL, and the checks on what a file holds (regular file, size limit,
UTF-8). Those checks move to a small shared internal package, so skills and the
workbench apply one set of rules.

Further rules:

- **`.git`.** It is skipped by `list_files` and `search_files`, and it can be
  read by `read_file`, because a reviewer may want `.git/HEAD`. It can never be
  written. A `.git` *file*, as in a worktree, is treated the same way. A name
  check alone is not enough. `docs -> .git` would reach it through a symlink,
  `.GIT` would on a case-insensitive volume, and `GIT~1` would through a Windows
  short name. The write path below therefore checks directory *identity*.
- **The write path follows no symlinks.** Reads follow symlinks that stay
  inside the root. `write_file` and `edit_file` do not, because `Root.Rename`
  replaces a terminal symlink rather than its target. An edit through
  `link -> real.go` would otherwise read `real.go` and then silently replace
  `link` with a regular file, destroying the link. Target editing done safely
  would have to pin the link's target across the read and the rename, which
  `os.Root` cannot do portably. So both write tools refuse:
  - **A terminal symlink** is refused with `file_is_symlink`. When the link's
    target, read with `Root.Readlink` and resolved inside the root, is a
    workspace path, the result names it, so the model can edit the real file.
    Nothing is read through the link and nothing is replaced.
  - **A symlink in any directory component** is refused with
    `path_through_symlink`.

  Both are enforced race-free by walking the path one directory at a time,
  holding handles:
  1. Start from the root handle.
  2. For each component, `Lstat` it through the current handle. It must be a
     directory, not a symlink.
  3. Open it with `OpenRoot` and check with `os.SameFile` that the opened
     directory is the one `Lstat` saw. A component swapped for a symlink
     between the two steps opens something else and is refused.
  4. The opened directory must also pass the mount check, and must not be the
     same file as the root's `.git` directory (compared once per step with
     `os.SameFile`). The identity check catches `.git` under any spelling.
  5. The temporary file, the rename, the directory sync and the crash cleanup
     all run on the final directory handle. A later swap of any name above it
     cannot move the write elsewhere.

  For the terminal name, `write_file` and `edit_file` `Lstat` it through the
  final handle. A symlink is refused there. For a regular file, they open it
  non-blocking and check `os.SameFile` against the `Lstat` result, then read
  (for `edit_file`) and take the mode from that same handle. The rename can
  still replace a symlink someone creates at that name after this check. That
  is no escape: it removes a name the actor made a moment earlier, and never
  writes through it. The result then reports that the target changed during
  the write. Directories that `write_file` creates are made one at a time with
  `Mkdir` on the current handle, then opened and checked like the others.

  Tests:
  - an edit and a write through a terminal symlink, both refused, with the
    link and its target unchanged;
  - a write through a symlinked directory, refused;
  - a write to `.GIT/config` on a case-insensitive volume, refused, and to
    `GIT~1\config` on Windows where short names are enabled (the test skips
    through `testenv` when the volume has them off);
  - a concurrency test that swaps a directory component for a symlink to a
    sibling of `WorkDir` while `write_file` runs in a loop. Nothing is ever
    written outside, and every call either writes inside or is refused.
- **Writes never modify a file in place.** `write_file` and `edit_file` write a
  temporary file in the same directory through the root and then rename it over
  the target (see "Atomic replacement and crashes" below). A hard link inside
  `WorkDir` to a file outside it is therefore replaced, and the file outside is
  never written through. The final mode is set by the rule in that section,
  which never depends on the umask.
- **Hard links, from stage 1 on.** `os.Root` confines *names*, not inodes. A
  hard link inside `WorkDir` whose inode is a file outside it has an ordinary
  name inside the root, and reading that name reads the outside file. Such a
  link needs no command to exist. It may already be in the checkout: pnpm, for
  example, hard-links `node_modules` files to a store in the home directory. Or
  something outside the session may create it while the session runs: another
  session, a caller-hosted tool, or the operator. So the rule applies whenever
  any workbench is set, not only with `Commands`:
  - **The check is on the open handle, not the name.** Every path that returns
    a file's contents opens the file through the root and then checks the
    *opened* handle: its link count, and that it is a regular file. On Unix this
    is `f.Stat()`'s `Nlink`. On Windows it is `NumberOfLinks` from
    `syscall.GetFileInformationByHandle`, which is CGO-free. A small
    per-platform `linkCount(*os.File)` wraps both. A count above 1 refuses the
    file. Whatever is then read comes from the handle that was checked, so a
    link created or swapped after the check cannot change which inode is read.
  - **Every reading path follows it.**
    - `read_file` refuses such a file with `file_linked`.
    - `search_files` skips it and reports how many linked files it skipped.
    - `edit_file` refuses it before matching. Otherwise its
      `match_not_found`/`match_not_unique` answers would reveal whether a string
      occurs in the outside file.
    - `list_files` returns names only, so it lists the entry and marks it
      `linked`.
    - `write_file` may replace the name, because the rename never writes the
      outside inode. The result says the link was replaced.
  - **Skills are not affected.** The skill tools read from the caller's own
    skill directories, which no workbench tool or command writes, so
    `internal/skills` keeps its current checks.
  - **What remains.** A file whose only other name is removed after it is linked
    in, leaving a link count of 1, has become a workspace file. Anyone who can
    do that could as well have copied it in. `WorkDir`'s contents are the
    caller's responsibility, as the data caveat below says.
  - **Tests.** Include a pre-existing link to a file outside `WorkDir`, and a
    concurrency test. In it, a goroutine repeatedly creates and removes a hard
    link from an outside file holding a marker, and swaps a directory for a
    symlink, while `read_file`, `search_files` and `edit_file` run in a loop.
    No result may ever contain the marker. The tests run on macOS, Linux and
    Windows. Where the environment refuses to create a hard link, as a sandbox
    may, the test skips through `testenv.SkipIfRefused`, the helper main added
    for exactly this. CI sets `AGENT_HARNESS_TEST_NO_SKIP=1`, so there it fails
    instead and no coverage is lost silently. The same helper gates `mkfifo`,
    the Unix socket and the junction in the tests below.

  Commands have the same gap on a different side. The OS sandboxes decide by
  *path*, so a sandboxed command can read an outside inode through a link that
  something outside the sandbox placed in `WorkDir`. The library cannot prevent
  that, and the README states it together with the data caveat. What the
  library can prove is that commands cannot *create* such a link themselves. The
  canary's `link` check below shows that a command cannot hard-link a file from
  the hidden home directory into `WorkDir`.
- **Mount points, from stage 1 on.** A file system mounted below `WorkDir`
  would let the tools send another tree's contents to the provider. That tree
  could be a bind mount of `~/.ssh`, an NFS share, a disk image, or a FUSE
  file system. The policy: **the file tools never leave the mount `WorkDir` is
  on.** When the session opens, the library records the root handle's mount
  identity. Every handle the tools open, **directories included**, is compared
  with it after the open and before anything is read from or through it:
  - **Linux:** the mount ID, from `unix.Statx(fd, "", AT_EMPTY_PATH,
    STATX_MNT_ID)` (golang.org/x/sys, already a dependency, CGO-free). A bind
    mount of a directory from the *same* file system has the same `st_dev` but
    a different mount ID, which is why the device number alone is not enough.
    On kernels before 5.8, which lack `STATX_MNT_ID`, the ID is read from
    `mnt_id:` in `/proc/self/fdinfo/<fd>`, which describes the same open file.
    If neither is available, the workbench is refused before the session starts
    (`workbench_mount_check_unavailable`), never run without the check.
    Stage 1 confirms that the pinned golang.org/x/sys (v0.28.0) exports
    `Statx`, `STATX_MNT_ID`, `GetFinalPathNameByHandle` and `GetFileType`, and
    bumps it as a released dependency if it does not. That could not be
    compiled here while writing.
  - **macOS:** `st_dev` from `f.Stat()`. macOS has no bind mounts. Every
    mount, whether a disk image, SMB, NFS, a macFUSE bind (`bindfs`) or a
    devfs, has its own device number. APFS firmlinks join the system and data
    volumes only at fixed system locations, never below a user's workspace.
  - **Windows:** the volume serial number and the final path. The volume
    serial comes from `windows.GetFileInformationByHandle`, and must equal the
    root's, so a volume mounted in a folder is caught. The final path comes
    from `windows.GetFinalPathNameByHandle` and must lie under the root's own
    final path. That catches a junction or a directory symlink to elsewhere on
    the same volume, whether or not `os.Root` already refused it.

  Every path uses the same check:
  - `read_file` and `edit_file` refuse a file on another mount with
    `file_other_mount`;
  - `list_files` lists a mount point's name, marked `mount`, and does not
    descend into it;
  - `search_files` skips it and reports how many mount points it skipped;
  - the write tools refuse a target whose directory handle is on another mount,
    before creating any temporary file there;
  - the crash cleanup does the same, so it never removes names on another
    mount.

  A directory walk opens each subdirectory through the root and checks the
  opened handle before it reads its entries. Everything below a refused
  directory is then out of reach, even if the directory is swapped after the
  check: the names are read from the handle that was checked. So is every
  file, whose content is read only from the handle that passed the check.

  **Coverage.** Unit tests feed the comparator handles from distinct mounts
  that every CI runner has without privileges:
  - on Linux, the temporary directory against `/proc/self` and `/dev/shm`;
  - on macOS, the temporary directory against `/dev`;
  - on Windows, a directory junction to a sibling of the root.

  A Linux CI step then uses `sudo mount --bind` to bind a directory holding a
  marker file below a test `WorkDir`, and runs an environment-gated
  integration test. It checks that `read_file`, `list_files`, `search_files`
  and `edit_file` never return the marker, and that a concurrent loop
  replacing a plain directory with the bind mount never leaks it. The test
  is built only for Linux. It runs when the CI step's variable names the
  prepared mount, and otherwise skips, saying so. This is a missing fixture,
  not an environment refusal, so it is a plain `t.Skip` that
  `AGENT_HARNESS_TEST_NO_SKIP` does not turn into a failure. The CI step always
  sets the variable, so on Linux CI it always runs.

  **Commands.** bwrap's `--bind` is recursive, and Seatbelt matches paths, so
  sandboxed commands *can* read a tree that something outside the sandbox
  mounted below `WorkDir`. That is the same boundary as for hard links, and the
  README says so. Commands cannot *create* mounts:
  - under bwrap they hold no capabilities and cannot create a user namespace
    in which to gain any. That is enforced by the pinned controls in "Pinned
    bwrap and its privilege controls" below, and proved by the canary's
    `privilege` and `overlay` checks and by the adversarial CI test. Any mount
    made inside a namespace would be invisible to the library anyway;
  - on macOS the profile allows no mount operations and none of the disk
    arbitration or disk-image services.

  The canary's `mount` check proves the macOS case, as the keychain check does:
  a command must fail to attach a small disk image the probe made at a mount
  point in the workspace. LAH-3 records the exact witness.
- **Only regular files are opened for content, and no open can block.** A
  FIFO in `WorkDir` blocks an ordinary open-for-reading until a writer appears,
  and a stage 2 command can make one with `mkfifo`. Checking the handle after
  the open is too late, because the open itself never returns. The admitted
  call would then never settle, breaking the settlement rule in `AGENTS.md`.
  The design on each platform:
  - **Unix.** Every content open goes through `root.OpenFile(name,
    O_RDONLY|O_NONBLOCK|O_NOCTTY, 0)`. `O_NONBLOCK` makes the open of a FIFO
    return at once, with or without a writer. `O_NOCTTY` means that opening a
    terminal cannot make it the process's controlling terminal. The opened
    handle is then checked: `f.Stat()` must report a regular file, or the
    handle is closed and the call answers `file_not_regular`. `O_NONBLOCK` has
    no effect on reads from a regular file. A `Root.Lstat` before the open is
    kept only as a cheap pre-filter that spares opening an obvious FIFO,
    socket or device. It is not relied on, because the name can change between
    the `Lstat` and the open, and the handle check is what decides.
    Opening a Unix socket fails (`ENXIO`) and is also `file_not_regular`.
    Device nodes cannot be created without privilege (`CAP_MKNOD`, or root on
    macOS), and a name pointing at `/dev` is refused as a symlink escape or by
    the mount check. A device inside `WorkDir` could therefore only have been
    put there by root, and even then the non-blocking open does not wait on it.
    Directories are opened with `O_DIRECTORY|O_NONBLOCK` and checked in the same
    way.
  - **Windows.** There are no FIFOs in the file system. Named pipes live in
    `\\.\pipe\`, which a root-relative name cannot reach. Two things are
    refused:
    - device names, which Win32 reinterprets: every path element that is a
      reserved name (`CON`, `PRN`, `AUX`, `NUL`, `COM0`–`COM9`, `LPT0`–`LPT9`,
      with any extension, or with trailing dots or spaces) is refused while the
      model's input is cleaned. This rule applies on every platform, so a path
      means the same thing everywhere;
    - any other handle whose `windows.GetFileType` is not `FILE_TYPE_DISK`,
      checked on the opened handle.
  - **Every path that opens something follows these rules:**
    - `read_file`, `search_files` and `edit_file` open for content;
    - `list_files` opens only directories;
    - `write_file` refuses an existing target that is not a regular file
      (checked on an `O_NONBLOCK` handle), rather than silently replacing a
      FIFO. Its temporary file is created with `O_CREATE|O_EXCL` and so can
      never open an existing FIFO;
    - the crash cleanup removes by name and never opens.
  - **Anything else that could stall.** A regular file on a stalled network
    or FUSE mount can still block a system call. The mount rule rules that out
    below `WorkDir`, but not for `WorkDir`'s own mount. No Go system call on a
    regular file can be interrupted, so `AGENTS.md`'s rule decides the design:
    a call is settled only when its work has actually stopped, and a new turn
    is admitted only after that. Returning early while the I/O goes on would
    admit the next turn while the work continued, and each repetition would
    leave one more goroutine and handle behind. So:
    - **One tracked worker at a time.** Every file tool, reading or writing,
      does its file-system work on the session's single *workspace worker*: a
      goroutine the session starts and tracks. Calls are already serialized by
      the loop, so one worker suffices, and at most one system call is ever
      outstanding.
    - **A cancelled call settles when the worker returns.** On cancellation
      the handler stops issuing new steps (the read tools stop between files
      and chunks, the write tools between the steps above) and waits for the
      worker's current system call. When that returns, the handle is closed
      and the call settles with `context.Canceled`. A write stops only at step
      boundaries. Cancelled before the rename, it removes its temporary file and
      reports that nothing changed. Once past the rename it does not stop: it
      finishes step 4 and reports its result, `write_outcome_unknown` included,
      exactly as in "Failures the tool itself sees".
    - **A bounded wait, then the session fails closed.** If the worker has not
      returned within a fixed grace of 10 seconds, the call is not reported as
      settled. The session fails with a typed `workspace_io_stuck`, through the
      same `failed` path a transcript write failure takes: the tool host is
      closed, no further call or turn is admitted, and the session reports the
      stuck call rather than claiming it ended. Because the session stops
      admitting work, a stuck worker can never be followed by a second one.
      There is at most one per session, and it is never multiplied by
      repeated cancellation.
    - **`Close` waits for the worker the same way.** It waits up to the grace.
      If the worker is still stuck, `Close` releases the transcript lock and
      returns the `workspace_io_stuck` error: the one goroutine it could not
      stop, and its handle, remain until the system call returns, which is
      when the worker exits. `Close` never reports that work stopped when it
      did not. The crash cleanup at `Open` and `Resume` runs on the same
      worker, under the same bound, and a stuck cleanup fails the `Open`.
    - **Commands are unaffected.** `run_command` is a process tree that can be
      killed, and the existing containment and `WaitDelay` already settle it.
  - **Tests.**
    - A test hook holds the worker's system call. Cancelling the call must not
      settle it while the hook holds, and releasing the hook within the grace
      settles it with `context.Canceled`.
    - A hook held past a shortened test grace fails the session with
      `workspace_io_stuck`. No later call or turn is admitted, `Close` returns
      the same error, and once the hook is released the worker exits: a
      goroutine counter returns to its baseline.
    - Cancelling a stalled read, releasing it, and calling again, repeated 100
      times, ends with exactly one worker goroutine and no open handles beyond
      the root.
  - **Tests.** Tests on macOS and Linux:
    - a FIFO with no writer, which `read_file`, `edit_file` and `write_file`
      refuse with `file_not_regular`, which `search_files` skips and
      `list_files` marks `fifo`. Each call must finish within a two-second
      deadline;
    - a concurrency test that repeatedly swaps a regular file for a FIFO of
      the same name while the read tools run in a loop. No call may exceed the
      deadline;
    - a Unix socket in `WorkDir`, refused;
    - the handle check fed an opened `/dev/null` directly, because a test
      cannot create a device node without root.

    On Windows, tests check that reserved names are refused on every platform,
    and that an opened `NUL` handle fails the `GetFileType` check.

  **What a returned byte is.** Together, these rules mean that every byte a
  file tool returns comes from a regular file:
  - reached by a name inside `WorkDir`, through `os.Root`;
  - on the same mount as `WorkDir`;
  - with one link;
  - read from the handle that was checked.

  That is the whole claim. It is not "nothing that ever came from outside
  `WorkDir`": anything someone copied in is a workspace file.
- **Bounds.** They are chosen so that a single call cannot fill the history,
  which Chat Completions resends in full on every step:

| Tool | Input | Bounds |
| --- | --- | --- |
| `read_file` | `path`, optional `offset` (line), `limit` (lines) | At most 2,000 lines and 64 KiB per call, with a note to continue from a line; files over 16 MiB, non-regular files and files that are not UTF-8 are refused |
| `list_files` | optional `path`, `depth` (default 2, max 8) | At most 2,000 entries, sorted, directories marked, `.git` skipped, truncation stated; symlinks listed as links and never followed |
| `search_files` | `pattern`, `literal` flag, optional `path`, `glob` | RE2 (`regexp`, linear time); at most 200 matches, each line cut to 400 bytes; files over 1 MiB and files with a NUL in their first 8 KiB skipped; `.git` skipped; truncation stated |
| `write_file` | `path`, `content` | Content at most 1 MiB, UTF-8; parent directories created inside the root |
| `edit_file` | `path`, `old`, `new`, optional `replace_all` | `old` must occur exactly once, unless `replace_all` is set; the file is under the `read_file` limits |

`.gitignore` is not interpreted in stage 1. `path` and `glob` narrow a search
instead. Honouring ignore files is a possible later addition, not a
dependency.

### Atomic replacement and crashes

`write_file` and `edit_file` replace a file in this order, all through the
root:

1. Create the temporary file in the target's directory, because a rename is
   atomic only within one file system. Its name is
   `.harness-workbench-<session>-<random>.tmp`, where `<session>` is the
   session ID's 32 hex digits, without its hyphens (IDs are the UUIDs
   `validSessionID` admits), and `<random>` is 16 random hex digits. It is
   created with `O_CREATE|O_EXCL` and mode 0600, so it never replaces anything
   and no one else can read it while it is written.
2. Write the content and `Sync` the file.
3. Set the final mode on the temporary file's own handle (`f.Chmod`), then
   `Rename` it over the target through the final directory handle. The file is
   0600 until this step, so the target's name never shows content with wider
   permissions than its final ones.
4. `Sync` the directory, opened through the root. When `write_file` created
   parent directories, each new directory's parent is synced too, deepest
   first. Only then does the tool return success.

**The final mode is deterministic.** `chmod` ignores the umask, and reading
the umask means changing it for the whole process, which races every other
goroutine. So the umask is never read, and the final mode comes only from what
is on disk or what the caller configured:

- **An existing file keeps its permission bits exactly,** execute bits
  included, taken from the handle the edit read. Set-user-ID, set-group-ID and
  sticky bits are always cleared, so an edit can never leave a set-ID file
  holding content the model wrote.
- **A read-only file is refused.** A file without its owner-write bit (Unix)
  or with the read-only attribute (Windows) is refused with `file_read_only`,
  rather than replaced by a rename that permissions would not otherwise allow.
- **A new file gets `Workbench.NewFileMode`,** which defaults to 0600. Only
  permission bits within 0666 are accepted, with no execute bits; anything
  else is refused at `normalizeAPI` (`RefusedLimit`). The confidential default
  never exposes more than the temporary file did. A caller that wants 0644 for
  a shared checkout says so. Git records only the execute bit, so the default
  does not change what a commit contains. Because the mode decides who can
  read the files the model creates, the effective mode (0600 when unset) is a
  `Ref` digest input whenever `Write` is set, as "The `Ref` digest" above
  describes; a resume under a different mode is refused. `normalizeAPI`
  resolves the default first, so the digest never sees 0. The mode is fixed
  when the session opens and cannot change mid-run, so a resume after a crash
  has to match it too, and the `Resume`-time cleanup of temporaries runs only
  after the digest matches.
- **Directories created by `write_file`** get `NewFileMode` with an execute
  bit added wherever a read bit is set (0600 becomes 0700, 0644 becomes 0755),
  set on the new directory's handle.
- **Windows** has no permission bits beyond the read-only attribute. A new
  file and its temporary both take the directory's inherited ACL, so the
  temporary file is exactly as confidential as the final file will be, and
  never less. `NewFileMode` has no effect there, which its documentation and
  the `WorkspaceWrite` reason say. It is still a digest input there, so the
  resume rule is the same on every platform. An existing file's read-only
  attribute is handled by the refusal above.

Tests:

- an edited 0755 script stays 0755, and an edited 0640 file stays 0640;
- a set-ID file loses its set-ID bits;
- a new file is exactly `NewFileMode` under umasks 000, 022 and 077. Each runs
  in a re-executed test binary that sets its own umask, so no test changes the
  umask of the process running the other tests;
- a pause hook between steps 2 and 3 observes the temporary file at 0600;
- a read-only target is refused on every platform;
- on Windows, a new file is writable and inherits its directory's ACL;
- resume mismatch: a `Write` session created with `NewFileMode` 0600 and
  resumed with 0644 fails with `errors.Is(err, ErrIncompatibleResume)`, and the
  transcript's bytes are unchanged;
- an unset `NewFileMode` resumes a session created with an explicit 0600, and
  the reverse, with an identical `ConfigHash`;
- a new golden in `session/ref_golden_test.go` pins the digest of a `Write`
  workbench with the default mode, and the existing goldens stay the same;
- a read-only workbench's digest does not change with `NewFileMode`.

The durability boundary follows from that order:

- **The result is recorded.** Once the transcript records the tool's success,
  the new content is on disk and its name is durable on macOS and Linux.
- **Windows** cannot sync a directory from Go. There the replacement is atomic
  (`MoveFileEx` with `REPLACE_EXISTING`), but it may not survive a power loss
  that follows at once. The Windows support reason says so.
- **A crash before step 3** leaves the target untouched and may leave the
  temporary file.
- **A crash between steps 3 and 4** may leave either the old or the new content
  after a power loss.

In both crash cases the loop already treats the call as having an unknown
outcome, and the model is told so. The call is never run again.

**Failures the tool itself sees** divide at the rename. The rename is the only
step that changes what the target holds, and nothing after it can undo it:

| Failure | What is true | Result to the model |
| --- | --- | --- |
| In step 1 or 2, or in step 3's `Chmod` | The target is untouched | A definite error (`write_failed`): nothing changed. The temporary file is removed first |
| `Rename` returns an error | Usually untouched, but an I/O error may come after the change | The tool decides from the directory handle, not the error. If the target is now the same file as the temporary (`os.SameFile`), the rename happened, and it goes on to step 4. If the temporary still exists under its own name and the target is not it, nothing changed: definite `write_failed`, and the temporary is removed. Anything else is `write_outcome_unknown`, with nothing removed |
| Step 4's directory `Sync` fails, or a new parent directory's `Sync` fails | The new content **is** in place and visible now, but may not survive a crash or power loss, which would bring back the old content or the new, never a mixture | `write_outcome_unknown`: "the new content was put in place but could not be made durable; after a crash the file may hold the old or the new content". Nothing is removed or restored, and there is no temporary left to remove |

`write_outcome_unknown` never implies a rollback. It is recorded and handled
as follows:

- **It is recorded in the transcript** as the call's result, with an explicit
  `outcome: "unknown"` field. The record is made durable before the model
  sees the result, like every result. `Session.Recovered()`, and the
  unknown-outcome answer a resume gives for a call cut off by a crash, report
  it the same way. A caller auditing the session sees one kind of "may or may
  not have happened", whether a crash or a sync failure caused it.
- **It is reported as an error result** (`IsError`), with a fixed text naming
  the target's relative path and saying that it holds the new content now, and
  that durability is unknown. The model can then read the file to see the
  current content, rather than assume either state.
- **The library never runs the call again,** and neither a resume nor the
  loop re-executes it. Whether to write again is the model's decision on its
  next step, as with any tool result, and a repeated write of the same content
  is harmless.
- **The failure is also a workspace fact for the caller.** A tool event
  carries the outcome, and `harness.ErrorFacts` on it gives family `turn` and
  code `write_outcome_unknown`, not retryable. The session continues: one
  unsynced directory is not a reason to stop, unlike a stuck worker, which is.

**Windows** has no step 4, so its only post-rename state is "atomic, not
known to be durable". That is stated in the support reason once, and is not
reported on every write.

Tests inject each failure through a fault-injection layer over the worker's
file operations:

- a failure in step 1, step 2 or the `Chmod`: definite `write_failed`, the
  target byte-for-byte unchanged, and no temporary left;
- a `Rename` that fails without changing anything: definite `write_failed`,
  and the temporary removed;
- a `Rename` that changes the target and then returns an error: the tool goes
  on to step 4 and reports success;
- a `Rename` whose outcome cannot be determined: `write_outcome_unknown`,
  with nothing removed;
- a directory `Sync` that fails after a successful rename:
  - the result is `write_outcome_unknown`;
  - the transcript records `outcome: "unknown"`;
  - the target holds the new content, which is not rolled back;
  - no temporary is left;
  - the library issues no second write;
- a simulated crash after that failure, where the layer may revert the
  uncommitted rename: `Resume` reports the call as unknown, and the target
  holds exactly the old or exactly the new content, never a mixture and never
  empty.

Abandoned temporary files are handled as follows:

- **Hidden from the tools.** The `.harness-workbench-*.tmp` pattern is
  reserved. `list_files` and `search_files` skip such files. `read_file`
  refuses them, and the write tools refuse them as targets, with
  `file_reserved`. This applies to every session's temporary files, whatever
  their session prefix, so a leftover from another or a concurrent session is
  never read, listed or searched.
- **Removed on the error path, before the rename only.** A failure in step 1,
  step 2 or step 3's `Chmod`, or a rename shown not to have happened, removes
  the temporary file before returning. Once the rename has happened there is no
  temporary to remove, and nothing is restored (see the table above).
- **Cleaned up after a crash.** When `Open` or `Resume` loads a transcript,
  it knows which `write_file` or `edit_file` calls were recorded without a
  result. For each one it removes this session's own temporaries (matching its
  session prefix) from the directory of the call's target, reached by the same
  symlink-free handle walk as the write path (a walk that is refused removes
  nothing). `Close` does the same for any call it had to abandon. It never removes
  another session's temporaries, because that session may be mid-write, and it
  never walks the whole workspace.
- **Scratch directories.** The `workbench/{home,tmp}` scratch below is also
  emptied at `Open` and `Resume`, because a crash skips `Close`.
- **Commands see the files.** A sandboxed command can list the temporary
  files, as it can anything in `WorkDir`. They hold only content the model
  itself asked to write.

Tests cover each step:

- the failure-injection cases listed above, before and after the rename;
- a transcript with an unanswered `write_file`, whose temporary file is removed
  at `Resume` while another session's is kept;
- the reserved pattern hidden from `list_files`, `search_files` and
  `read_file`.

## Stage 1: read-only workbench (LAH-2)

`Workbench{}` with neither `Write` nor `Commands` set gives a session
`read_file`, `list_files` and `search_files`, and no shell. This is enough for
the researcher, reviewer and PM roles. It is pure Go, so it works on macOS,
Linux and Windows alike. It starts no process, so it needs no sandbox and no
proof. Confinement is `os.Root`, plus the link-count check on each opened
file. The tests prove it with escapes: `..`, absolute paths, a symlink to a
sibling, a symlink chain, a directory swapped for a symlink, a hard link to a
file outside `WorkDir` (pre-existing and created concurrently), a mount below
`WorkDir`, a FIFO and a socket. Every byte returned meets "What a returned
byte is" above, and no call blocks on what it opens.

**Stage 1 lands in two parts.** The owner split it so that each part can be
reviewed on its own:

- **Part 1 (LAH-2), the inert foundation.** It adds:
  - `Options.Workbench`, the `WorkDir` and `RuntimeHome` rules and the six
    reserved names;
  - the `Ref` digest, and `workbenchDefinitions` feeding both `apiTools` and
    the direct host;
  - `read_file` and `list_files` on `os.Root`, with the bounds above;
  - the shared `internal/wsfile` checks, which `internal/skills` now uses
    unchanged.

  `normalizeWorkbench` runs every check and then still refuses the option
  with `RefusedNotOffered`. Only an unexported flag that tests set gets past
  that refusal. So `Support`, the README's planned note and CI do not change.
  `Workbench` has no stage 2 fields yet, so there is nothing to refuse for
  them. The `WorkDir` and `RuntimeHome` comparison uses `os.SameFile` on every
  platform, not only on macOS and Windows. That is stricter, and it costs
  nothing on Linux. A read tool that cannot be answered says so with a fixed
  code. Besides the codes named above, part 1 uses `file_path_invalid`,
  `file_not_found`, `file_not_directory`, `file_unreadable` and
  `arguments_invalid`. Two more rules keep a result honest:
  - **Partial listings.** A directory that fails part-way, cannot be opened,
    or has gone by the time it is opened is never shown as complete.
    `list_files` lists what it read and adds a note giving how many
    directories could not be read in full. Only a failure before any entry of
    the requested directory itself is read is an error.
  - **Entry types.** `list_files` reads names only, and types each entry with
    `Lstat` through the root: the entry itself, never a link's target. So a
    file system that leaves a directory entry's type unknown cannot make a
    directory look like a file. A directory is descended by opening it and
    checking the opened handle. Anything else is never opened, so part 1
    cannot block on a FIFO it lists.
  - **A bounded walk.** Names are read in batches of 256 and handled as they
    arrive, so one directory is never held whole. Every name read counts
    towards a limit of 20,000 per call, `.git` and the reserved temporaries
    included. At that limit the walk stops and says so. Cancellation is
    checked at every name.
  - **A walk through handles.** Neither tool opens a path longer than one
    name. Each path is walked one component at a time: `Lstat` through the
    parent directory's handle, then `OpenRoot` through that same handle, and
    the opened directory must be the one `Lstat` saw (`os.SameFile`). So an
    ancestor swapped for a link after it was checked is caught at that
    ancestor. It is never followed, even when the link leads to a look-alike
    inside the workspace. A cursor keeps the directories along the current
    path open and reuses the prefix that the next path shares, so a
    breadth-first listing opens about one directory per directory it lists.
    It holds at most one handle per level.
  - **Hidden names, by what is opened.** Each directory entered is also
    judged by identity against its parent's `.git` (`os.SameFile`). On
    Windows it is judged by the name the file system gives the opened handle
    (`GetFinalPathNameByHandle`, which returns long names), and so is the
    file `read_file` opens. So an 8.3 short name such as `GIT~1` cannot
    unhide `.git` or a reserved temporary, whether as the target or as an
    ancestor. Elsewhere a name reaches a file only as spelled, up to case,
    which the checks already ignore.
  - **The path asked for.** `list_files` follows no link at all. A link is
    refused: `file_is_symlink` when it is the last component,
    `path_through_symlink` on the way. That applies even to a link that stays
    inside. A `.git` component (in any case, trailing dots and spaces
    ignored) and a reserved temporary name are refused with `path_not_listed`,
    whether it is spelled that way or only opens as that.

    `read_file` still follows links that stay inside and still reads `.git`,
    but it resolves each link itself. It reads the link through its
    directory's handle, refuses an absolute target or one that leaves the
    workspace (`file_outside_workspace`), and walks the rest again from the
    root through the same checks, for at most 40 hops. A reserved temporary
    name is refused in any component, of the path asked for or of any link's
    target, and the file it opens must be the one `Lstat` saw.
  - **A bounded read.** `read_file` finds line ends by scanning the file's
    bytes in place, and copies only the part it shows. So a 16 MiB file of
    one-byte lines costs about what a result holds, not a string per line.
    A line longer than a result is cut on a character boundary.
  - **The result limit.** A session with a workbench needs
    `ToolHost.MaxResultBytes` of 0 (64 KiB) or at least 4 KiB (`RefusedLimit`).
    An error repeats at most 256 bytes of the model's path. So every result,
    with its notes, fits without the host cutting it. The host's own
    truncation note now counts towards `MaxResultBytes` for every tool. The
    limit applies to every answer to a call: a result, a handler's error, a
    cancellation, a refusal, and the loop's not-run and unknown-outcome
    answers. So nothing the model is answered with exceeds it.
- **Part 2 switches it on.** It adds:
  - the opened-handle checks (link count, mount identity, `GetFileType`) and
    the `O_NONBLOCK|O_NOCTTY` opens;
  - the workspace worker, with `workspace_io_stuck`;
  - `search_files`;
  - the `Support` entries, the README section with its data caveat, and the
    Linux CI bind-mount step.

  Last, it removes the flag. Until then no caller can reach a tool that lacks
  those checks.

**Data caveat.** Anything a read tool returns goes to the model provider. With
OpenRouter that is a third party chosen by routing (see below). `WorkDir` is
therefore also the edge of what may leave the machine. The README will tell
callers to use a workspace that holds no secrets, such as a clean checkout
without `.env` files, and that nothing outside the session should link
outside files into it.

## Stage 2: edits and sandboxed commands (LAH-3)

### 2a: write and edit

`Write: true` adds `write_file` and `edit_file`, confined as described above.
They are pure Go and are offered on every platform, including Windows. A
documentation or planning role can then write without commands.

### Commands, common to macOS and Linux

`Commands` adds `run_command` with the input `{"command": string, "dir":
optional relative path, "timeout_seconds": optional}`.

- The command string runs as `/bin/sh -c <command>`, **inside** the sandbox.
  The shell is the model's tool, not the library's, so it is sandboxed with
  everything it starts.
- **Working directory:** `WorkDir`, or `dir` resolved through the root.
- **Environment:** `skills.Environment`'s allowlist (`PATH`, `LANG`, `LC_*`),
  plus `Commands.Env` and the process package's launch marker. `HOME` and
  `TMPDIR` point at a private scratch directory,
  `RuntimeHome/sessions/<id>/workbench/{home,tmp}`, created with mode 0700. It
  is kept for the session's life and removed at `Close`. Caches such as Go's
  build cache then land in a writable place that is not the workspace.
- **Timeout:** `Commands.Timeout` defaults to 2 minutes and may be at most 10
  minutes (`skills.MaxTimeout`). The model may ask for less, never more. A
  timeout returns an output with `timed_out: true`, not an error.
- **Output:** exit code, stdout and stderr, each bounded at 64 KiB
  (`skills.MaxOutputBytes`, with a truncation note). `Cmd.WaitDelay` closes the
  pipes shortly after the shell exits. A server the command started in the
  background, which still holds the pipes, therefore cannot keep the call open.
  Its later output is discarded.
- **Containment:** `process.Command` gives each command its own process tree,
  and `ctx`, the timeout or an interrupt ends that whole tree.
  `Options.Background`, refused for API sessions today because they start no
  process, becomes available with `Commands` and runs each command at nice 10,
  as it does for the CLIs. At `Close`, the per-launch marker
  sweep (`sweep_darwin.go`, `sweep_linux.go`) stops whatever the session's
  commands left running. The existing loop already runs calls one at a time,
  records each call before it runs and its result after, and answers a call cut
  off by a crash as having an unknown outcome instead of running it again. A
  command is never run twice.
- **Skill scripts** (`run_skill_script`) are unchanged. They run where
  `SkillRun` says, outside this sandbox, as today. Running them inside the
  workbench sandbox is a possible follow-up, not part of this design.

### What commands may read

A command's output goes to the model provider, so anything a command can read
can leave the machine. Hiding only the operator's home would still leave
readable secrets and host state elsewhere:
- other users' homes where their permissions allow it;
- `/root` on a lax host;
- `/var/log`, `/var/lib`, `/srv`, `/opt`, `/mnt`, `/media`;
- world-readable tokens in `/etc`;
- `/Library` and `/private/var` on macOS.

So reads are an **allowlist on both platforms**, not "everything except home".
A command may read exactly:

1. **The system runtime set,** pinned in the library per platform and listed
   in its documentation. It holds what a shell, a compiler and a test runner
   need to start, and nothing that is host-private:
   - **Linux:** `/usr`, `/bin`, `/sbin`, `/lib`, `/lib32`, `/lib64` and
     `/libx32`. On a merged-`/usr` system, each of these that is a symlink on
     the host is recreated with `--symlink`, not bound. Also these files and
     directories from `/etc`: `passwd`, `group`, `nsswitch.conf`, `hosts`,
     `localtime`, `ld.so.cache`, `ld.so.conf`, `ld.so.conf.d`, `ssl/certs`,
     `ca-certificates`, `alternatives` and `os-release`. Nothing else from
     `/etc`, and no `/var`, `/opt`, `/srv`, `/mnt`, `/media`, `/root`, `/home`,
     `/sys` or `/run`. `/proc` is the sandbox's own, and `/dev` is bwrap's
     minimal device set.
   - **macOS:** `/System`, `/usr`, `/bin`, `/sbin`,
     `/Library/Developer/CommandLineTools`, the selected Xcode (the resolved
     `xcode-select -p` developer directory, read by the library outside the
     sandbox at session start), and `/opt/homebrew` or `/usr/local`, where
     Homebrew lives. From `/private/etc` it allows the same list of files as on
     Linux, where they exist. It also allows `/dev/null`, `/dev/zero`,
     `/dev/random`, `/dev/urandom`, `/dev/fd` and the process's own tty.
     `file-read-metadata`, but not `file-read-data`, is allowed on the literal
     ancestors of each allowed path, so tools can resolve and `getcwd` their
     way to them without listing anything.
2. **`WorkDir`.**
3. **`Commands.Read`.** The caller names here toolchains or caches that live
   elsewhere: `~/go/pkg/mod`, a Nix store, `/opt/toolchain`. It stays subject
   to the home and `RuntimeHome` refusals above.
4. **This session's scratch** (`home` and `tmp`).

Everything else is unreadable, with no deny list to keep up to date. On macOS
it is refused by `(deny default)`. On Linux it simply does not exist in the
sandbox, because the new root is an empty tmpfs that holds only what is bound
into it (see "Mount layout"). The system set may grow only through a library
release that names the addition and its reason in the design and the
changelog. A toolchain the set does not cover fails visibly inside the
command, with "not found" or "permission denied", which is safer than a silent
read, and the README says to add its directory to `Commands.Read`.

**Proved by the canary.** The probe places a marker file in each of several
locations outside the set and requires every read of them to be refused:
- a directory beside the workspace;
- a stand-in home;
- a stand-in `RuntimeHome`;
- a directory standing in for a host-private location such as `/var/lib`.

On Linux it also requires `/etc/<a file outside the list>`, which the probe
chooses from the real host, to be absent inside the sandbox. It requires the
system set to work: `/bin/sh` runs, and `ls /usr/bin` and reading
`/etc/passwd` succeed. And it requires a `Commands.Read` directory to be
readable and not writable. The widened-rule negative test adds `/` to the set
and must fail `outside`.

### 2b: macOS, Seatbelt

Each command runs as `/usr/bin/sandbox-exec -p <profile> /bin/sh -c
<command>`. The profile is generated for each session from a template pinned in
the library. Its baseline is `(deny default)`, with allows modelled on the
open-source sandbox runtime that Claude Code uses, and LAH-3 justifies every
allow in a comment:

- **Reads:** only the read set above. Everything else falls to `(deny
  default)`, including the home directory, other homes, `/Library`,
  `/private/var` and `RuntimeHome` outside this session's scratch. Rule order
  is irrelevant because nothing is allowed broadly and then carved out. A
  `WorkDir` that is the home directory or contains it is refused when
  `Commands` is set, as on Linux.
- **Writes:** allowed only in `WorkDir` (when `Write` is set), the scratch
  `home` and `tmp`, and `/dev/null` and the standard ttys. Writes to
  `WorkDir/.git` are denied after the `WorkDir` allow, so `.git` is read-only.
  So are renaming or unlinking `.git` itself, and creating a hard link whose
  source is inside `.git`. Seatbelt checks the resolved path, so a symlink to
  `.git` gains nothing. The canary's `gitmove`, `gitlink` and `githardlink`
  checks prove each one, and LAH-3 names the exact profile operation for the
  hard link in a comment.
- **Network:** with `Loopback` off, all `network*` operations are denied. That
  also covers `AF_UNIX` connects, so the ssh and gpg agents cannot be reached.
  With `Loopback` on, only binding, inbound and outbound connections for
  `localhost` IP addresses are allowed.
- **Mach services:** only the lookups that ordinary command-line tools need,
  such as name resolution. They are enumerated, and `com.apple.SecurityServer`
  and the other keychain services are never among them.
- `process-exec`, `process-fork` and `signal` are allowed only for the command's
  own processes (`(target same-sandbox)`).

`sandbox-exec` is deprecated in its man page, but it is still shipped and used
by Claude Code and Codex. If a macOS release removes it, the session is refused
with `sandbox_tool_missing`. It never falls back to running unsandboxed.

### 2c: Linux, bubblewrap (required)

The owner has decided this: **bubblewrap is required on Linux**, as it is for
Claude Code's sandbox. There is no Landlock or seccomp helper, and consumers add
no hook to their `main`. Each command runs as:

```
bwrap --unshare-user --disable-userns --cap-drop ALL   # privilege controls (below)
      --unshare-net --unshare-pid --unshare-ipc --unshare-uts --unshare-cgroup
      --die-with-parent --new-session
      --tmpfs /                                   # phase 1: an empty new root
      --dev /dev --proc /proc
      --tmpfs /tmp --tmpfs /run                   # phase 1: every covering tmpfs
      --perms 0755 --dir /usr --perms 0755 --dir /etc --perms 0755 --dir /etc/ssl ...
      --perms 0700 --dir <each workspace parent>  # phase 2: every directory, shallowest first
      --ro-bind /usr /usr --symlink usr/bin /bin  # phase 3: the system set
      --ro-bind /etc/passwd /etc/passwd ...       #   (onto phase-2 directories)
      --ro-bind <each Commands.Read> <same>       # phase 4: binds, shallowest first
      --bind|--ro-bind $WORKDIR $WORKDIR          #   --bind only with Write
      --bind <scratch home> <same> --bind <scratch tmp> <same>
      --ro-bind $WORKDIR/.git $WORKDIR/.git       # phase 5: read-only overlays
      --chdir <dir> -- /bin/sh -c <command>
```

**Mount layout.** One function, `bwrapArgs(layout)`, builds these arguments
for both the session and the canary. `layout` holds the resolved work
directory, scratch paths, read directories and the pinned system set. There is
no `--ro-bind / /` and no tmpfs laid over home or `RuntimeHome`. A path is
hidden simply by not being bound into the empty root. The function emits its
operations in **five fixed phases**, and no operation from an earlier phase
ever follows one from a later phase:

1. **Every tmpfs.** The new root, then `/tmp` and `/run`, plus `--dev` and
   `--proc`. These are the only mounts that can cover a path, and all of them
   come first, so nothing mounted later is ever covered by a tmpfs mounted
   after it. The builder rejects, as a programming error caught by its tests,
   any layout that would put a tmpfs after phase 1.
2. **Every directory any later operation needs, shallowest first.** The
   builder collects every destination of phases 3 to 5: system-set binds,
   system-set symlinks, `/etc` file binds, `Commands.Read`, `WorkDir` and the
   scratch. For each one it takes the destination itself, when that is a
   directory, and every ancestor, when that is a file or a symlink. Then it
   emits `--dir` for each such directory that phase 1 did not already provide,
   once each, sorted by depth and then by name. System directories (`/usr`,
   `/lib64`, `/etc`, `/etc/ssl`, …) get `--perms 0755`, and workspace parents
   (`/home/<user>/...`, `/var/lib/<app>/sessions/<id>/workbench`,
   `/tmp/...`) get `--perms 0700`. After this phase, every directory that
   anything is bound onto already exists. **No operation relies on bwrap
   creating a directory.** The only thing bwrap creates itself is the empty
   mount-point *file* for a file bind (`/etc/passwd`), inside a directory that
   phase 2 made. That is the documented behaviour of `--ro-bind` for a file
   source, which it cannot perform any other way, and it is present in every
   version the library accepts (see "Pinned bwrap" below).
3. **The system set,** bound read-only onto phase-2 directories or recreated
   with `--symlink`, with each symlink's parent made in phase 2.
4. **Binds,** ordered shallowest first (by depth, then by name). A read
   directory inside `WorkDir`, or `WorkDir` inside a read directory, is then
   never hidden by a shallower bind made after it.
5. **Read-only overlays:** `.git` over `WorkDir`'s writable bind.

**No directory is created inside a bind.** A `--dir` inside a read-only bind
would fail, and one inside a writable bind would write to the host. So a
workspace destination may not lie inside the system set: a `WorkDir`, a
scratch or a `RuntimeHome` under `/usr`, `/lib*`, `/bin`, `/sbin` or `/etc` is
refused (`RefusedWorkDir` or `RefusedRuntimeHome`, "inside the command
sandbox's system set"). A `Commands.Read` entry inside the system set is
redundant, so it is dropped with its duplicate bind. Workspace parents in phase
2 then only ever land on phase 1's tmpfs mounts, never on anything bound.

**Pinned bwrap and its privilege controls.** A `.git` overlay, or any other
mount, can be undone only by someone holding `CAP_SYS_ADMIN` over the mount
namespace. So the design removes every way a command could get that, and
proves it, rather than relying on bwrap's defaults:

- **`--unshare-user`,** always, even where bwrap is setuid. The command runs
  in a user namespace of its own, which bwrap does not own on the host.
- **`--disable-userns`** (bwrap 0.8.0 and later). The command cannot create a
  nested user namespace. Without this, `unshare -Urm` would hand it every
  capability inside a new namespace of its own, where it could try to unmount
  or rebind. Even then the kernel marks mounts inherited from a more
  privileged namespace as locked, so an overlay could not be removed on its
  own, and a non-recursive rebind of `WorkDir` that would expose the `.git`
  beneath it fails with `EINVAL`. But the design does not rest on that alone.
- **`--cap-drop ALL`,** stated explicitly, so the command's effective,
  permitted, inheritable and ambient capability sets are empty, whatever
  bwrap's defaults are.
- **`PR_SET_NO_NEW_PRIVS`,** which bwrap always sets. No set-UID or file
  capability inside the sandbox can raise privilege.
- **`--unshare-cgroup`** as well, so the cgroup tree is not shown.

The library therefore **requires bwrap 0.8.0 or later**, the first release
with `--disable-userns`. It reads the version with `bwrap --version` before
the trial run and refuses an older one with a new `sandbox_tool_outdated`
(`Tools: ["bwrap"]`, "bubblewrap 0.8.0 or later is required"). Debian 12,
Fedora 38+, Arch and Ubuntu 24.04 ship it. Ubuntu 22.04's 0.6.1 does not, so on
that release a newer bwrap has to be installed. The README states this
consequence of the owner's bubblewrap decision. The version goes into the
cache key.

Further rules:

- **Real paths.** Every path is resolved with `EvalSymlinks` before it is
  used, so binds name the real directories, and a system-set entry that is a
  symlink on the host is recreated as the same symlink.
- **No path may reopen home or `RuntimeHome`.** When `Commands` is set, a
  `WorkDir` that is or contains the home directory is refused (`RefusedWorkDir`,
  "the workspace would expose the whole home directory to commands"). That is
  the same rule `sandboxReadDirs` applies to `Commands.Read`, and together with
  the `RuntimeHome` rules above it means no bind can expose either one. The
  macOS profile refuses the same configurations.
- **The canary runs the session's layout shape.** The canary cannot bind the
  session's real paths without touching them. `layout` therefore takes a
  prefix map, and the canary nests its disposable workspace, scratch,
  `RuntimeHome` and read directories under **the same kind of covering location
  and at the same depth** as the session's real paths:
  - under the root tmpfs, for a real path such as `/home/...` or
    `/var/lib/...`;
  - under the `/tmp` tmpfs, for a real path under `/tmp`.

  It thereby exercises the same phase-2 chain and the same phase-4 order. The
  cache key includes that shape: for each path, its covering tmpfs and its
  depth.
- **Tests.**
  - Unit tests check the exact phase order. A test simulates the arguments
    against an empty tree and requires every bind, symlink and `--dir` to find
    its parent directory already created by an earlier argument, and every
    directory bind to find its destination already created. It also requires
    every `--dir` to follow every tmpfs and to precede every bind, and no
    `--dir` to lie inside a bind. It runs for each of these cases:
    - `WorkDir` inside home and outside it;
    - `RuntimeHome` (and so the scratch) inside home and outside it;
    - `WorkDir` under `/tmp`;
    - `Commands.Read` nested inside `WorkDir`, and `WorkDir` nested inside a
      read directory;
    - a merged-`/usr` host, with symlinked `/bin` and `/lib`;
    - `WorkDir` equal to home or containing it (refused);
    - `RuntimeHome`, `WorkDir` or the scratch inside the system set (refused);
    - a `Commands.Read` entry inside the system set (dropped).
  - The Linux CI job runs the **real** canary four times: with `RuntimeHome`
    inside `$HOME` and with it outside (under `/var/tmp`), each with
    `WorkDir` inside `$HOME` and under `/tmp`. Every run must pass. It runs
    them twice over: with the distribution's bwrap, and with bwrap 0.8.0 built
    from its release tarball, the oldest version the library accepts.
  - A Linux CI test re-executes its own test binary inside the session's exact
    bwrap arguments as an adversarial helper. The helper makes the raw system
    calls directly, so it does not depend on util-linux being installed:
    - `umount2(WorkDir/.git, 0)` and `umount2(…, MNT_DETACH)`;
    - `mount(…, MS_REMOUNT|MS_BIND, …)` without `MS_RDONLY` on `.git`;
    - a non-recursive `mount(WorkDir, /tmp/x, MS_BIND)`, followed by a write
      to `/tmp/x/.git/config`;
    - `unshare(CLONE_NEWUSER|CLONE_NEWNS)`, then the same three attempts
      inside it, if the unshare ever succeeded;
    - `capset` to raise any capability.

    Every one must fail, and the host's `.git/config` must be byte-for-byte
    unchanged afterwards. The same test run *without* `--disable-userns` and
    `--cap-drop ALL` must still show the unshare succeeding but the kernel's
    mount locking refusing the rebind. That records what each control
    contributes. The test is built only for Linux, and runs in CI on the
    distribution's bwrap and on bwrap 0.8.0.

- **Sockets.** `/run` and `/var/run` are never bound (the empty `/run` is a
  tmpfs), which hides the Docker socket, the
  systemd and D-Bus buses (and through D-Bus the Secret Service keyring), and
  `/run/user/<uid>` with the ssh and gpg agents. A read-only bind does not stop
  a Unix-socket `connect`, so hiding them is what closes them. `--tmpfs /tmp`
  hides the X11 sockets and other `/tmp` sockets. `--unshare-net` closes
  abstract sockets and every IP address.
- **Reads.** The empty root, filled only with the read set, gives the same
  read rule as the macOS profile.
- **Order matters.** A later mount covers an earlier one. The five phases fix
  the order, and the canary proves it took effect.
- `--new-session` stops `TIOCSTI` injection into a terminal. `--die-with-parent`
  and the PID namespace mean that when the shell exits, the kernel ends
  everything the command started. Unlike on macOS, nothing a command starts
  outlives the command.

**Refusals before launch.** These are new capability codes with fixed messages.
None of them ever falls back to running without a sandbox:

| Condition | Code | Error message names |
| --- | --- | --- |
| `bwrap` not found | `sandbox_tool_missing`, `Tools: ["bwrap"]` | bubblewrap, and `apt install bubblewrap`, `dnf install bubblewrap`, `pacman -S bubblewrap` |
| `bwrap` older than 0.8.0 | `sandbox_tool_outdated`, `Tools: ["bwrap"]` | bubblewrap 0.8.0 or later, needed for `--disable-userns`, and that the distribution's package or a newer build provides it |
| `bwrap` present but cannot create namespaces | `sandbox_namespaces_unavailable`, `Tools: ["bwrap"]` | unprivileged user namespaces: `kernel.unprivileged_userns_clone`, `user.max_user_namespaces`, and Ubuntu 24.04+'s `kernel.apparmor_restrict_unprivileged_userns` with its AppArmor profile for bwrap. A setuid bwrap is accepted only if the trial run with `--unshare-user --disable-userns` succeeds |

All three are `CapabilityError`s in the capability family with `Phase:
BeforeLaunch`, so a consumer app can show `Error()` as it is. The case is
detected by the library's own trial run of bwrap, with its exit status. bwrap's
stderr is never read into the error. `bwrap` is looked up on the library
process's `PATH`, and the binary's identity (path, size, mtime) goes into the
cache key below. On macOS, a missing `/usr/bin/sandbox-exec` also gives
`sandbox_tool_missing` with `Tools: ["sandbox-exec"]`.

### Proof before launch: the canary

This works as `probeClaudeSandbox` and `probeCodexSandbox` do. Before the
session starts, and before any credential is resolved, the library runs a
canary script under **exactly the command line and profile the session's
commands will use**, generated for a disposable workspace and scratch
directory. It reuses the existing outcome vocabulary and adds a few outcomes:

| Outcome | Must be |
| --- | --- |
| `inside` (write in the workspace) | allowed when `Write`, refused otherwise |
| `sibling` (write next to the workspace) | refused, and the file absent afterwards |
| `tmp` / `tmpdir` (write the scratch `TMPDIR`) | allowed |
| `gitdir` (append to `.git/config`) | refused |
| `gitmove` (rename `.git` aside, then write into the renamed directory) | refused, and `.git` still in place |
| `gitlink` (make a symlink in the workspace to `.git`, then write through it) | refused |
| `githardlink` (hard-link `.git/config` into the workspace, then append to the link) | refused: the link fails (across a mount on Linux, by the profile on macOS), or the write is refused |
| Linux: `privilege` (read `/proc/self/status`) | `CapEff`, `CapPrm`, `CapInh` and `CapAmb` all zero, and `NoNewPrivs: 1` |
| Linux: `overlay` (with util-linux where the system set has it: `umount` and `umount -l` on `.git`, `mount -o remount,rw,bind` on `.git`, `mount --bind` of the workspace to `/tmp/x`, and `unshare -Urm` wrapping each, each followed by an append to `.git/config`) | every step refused. A missing tool is recorded as not attempted, because the adversarial CI test makes the same calls without util-linux |

After every canary run the probe checks that the host's `.git/config` in the
disposable workspace is byte-for-byte unchanged, whatever the script reported.
A change fails the proof even if every line said "refused".
| `home` (read a file the probe placed in a stand-in home, beside the parents the layout created) | refused |
| `outside` (read marker files the probe placed beside the workspace and in a stand-in host-private directory; on Linux, stat an `/etc` file outside the list) | refused, or absent |
| `system` (run `/bin/sh`, list `/usr/bin`, read `/etc/passwd`) | allowed |
| `readset` (read a file in a `Commands.Read` directory, then try to write there) | read allowed, write refused |
| `link` (hard-link that stand-in home file into the workspace) | refused, and no link left behind |
| `runtime` (read a stand-in transcript in a stand-in `RuntimeHome`, beside the session's scratch) | refused, while the scratch stays writable |
| `nested` (write a file in the workspace, nested at the session's covering location and depth, when `Write`) | allowed, and the probe sees it on the real disk afterwards |
| `socket` (connect to a Unix socket the probe listens on in a hidden directory) | refused |
| `network` (reach `offMachineWitness`) | refused; the listener the probe owns must count no connection |
| loopback (`loopbackCanary`, only with `Loopback`) | reaches the probe's loopback listener and binds its own |
| `canary-ran` | printed last; without it the check failed |
| macOS: keychain (`security list-keychains -d user` returns nothing usable) | refused |
| macOS: `mount` (attach a disk image the probe made, at a mount point in the workspace) | refused, and nothing mounted afterwards |

The rules proved in the earlier sandbox work apply as they did there:

- the script writes distinct files, never the probe's own directory;
- a unit test runs the canary with **no** sandbox and requires every escape to
  be reported;
- a negative test widens one rule, such as allowing the network, and requires
  `sandbox_not_enforced`.

A failure is `CapabilitySandboxNotEnforced`. A canary that could not be run or
did not finish is `CapabilitySandboxUnavailable` or `CapabilityProbeTimeout`.

The result is cached in the existing `verified` set, keyed by what was
checked:

- `Kind: "workbench"`;
- the sandbox binary's identity;
- the digest of the profile template, which carries the library's version;
- `Write`, `Loopback` and `Read`;
- the pinned system set's digest;
- on Linux, bwrap's reported version;
- the layout's shape: for the workspace, the scratch, `RuntimeHome` and each
  read directory, its covering tmpfs, whether it is under home, and its depth.

The cache lives only in memory, so a sysctl change is seen by the next process.
The macOS keychain check needs a live confirmation that `list-keychains` is the
right witness, because a `(deny default)` profile may make it fail for reasons
other than the keychain. LAH-3 picks the witness and records it.

### Loopback

- **macOS:** the profile allows `localhost` only, and the canary proves it as
  Claude's does: the command reaches the probe's listener, binds its own, and
  is refused the off-machine witness. Servers outlive a command, so a server
  started by one command can be reached by the next. `Support` is `unknown`,
  proved before each launch.
- **Linux:** each `bwrap` has its own network namespace. A command can start a
  server and request it **within the same command**, for example `srv & sleep
  1; curl localhost:8080`. But the next command has a new namespace, and
  `--unshare-pid` has already ended the server. Sharing a namespace across
  commands would need a sandbox that lives for the whole session, with each
  command entered into it. bwrap can pass `--userns`/`--pidns` file descriptors
  but has no option for a network namespace, and entering with `nsenter` from
  the unsandboxed side needs its own proof. **Recommendation:** stage 2c
  refuses `Commands.Loopback` on Linux, with
  `Support = Unsupported, "each command runs in its own bubblewrap network
  namespace, so a server one command starts is gone before the next; a single
  command may still start and request its own server"`. A session-long
  namespace is a separate follow-up task (7 below). It is claimed only after a
  two-command canary shows that a second command reaches the first command's
  listener, and that neither command reaches the host's loopback. If LAH-3
  proves that design in time, it may ship it instead. The claim follows only
  what is proved.

### Windows

`Workbench{}` and `Write` are offered (pure Go, `os.Root`). `Commands` is
refused before launch with an `UnsupportedError` whose capability is
`Support(OpenAICompatible, Session, Sandbox)` on Windows: `Unsupported,
"Windows has no command sandbox the library can prove; the read and edit tools
remain available"`. A Windows command sandbox (AppContainer or a job object
with restricted tokens) is out of scope.

## `harness.Support` claims per stage

These are for `OpenAICompatible` / `Session`. The README's Support table
changes with each stage when that stage lands, not before.

| Feature | Today | Stage 1 | Stage 2 |
| --- | --- | --- | --- |
| `WorkspaceRead` (new, `"workspace_read"`) | — | `Composed`, "the library's read_file, list_files and search_files: regular, singly linked files reached inside WorkDir on WorkDir's own mount" | same |
| `WorkspaceWrite` (new, `"workspace_write"`) | — | `Unsupported`, "not offered yet" | `Composed`, "the library's write_file and edit_file, inside WorkDir on WorkDir's own mount; .git is never written; symlinks are refused, not written through; each write is an atomic, synced replacement"; on Windows the reason adds "not directory-synced; new files take the directory's ACL" |
| `Sandbox` | `Unsupported`, "no native tools to sandbox" | unchanged | darwin, linux: `Unknown`, "the library's run_command sandbox, proved before launch by its canary: reads limited to a pinned system set, WorkDir and Commands.Read; writes to WorkDir and scratch; Linux requires bubblewrap 0.8.0 or later, run without capabilities or nested user namespaces"; windows: `Unsupported` as above |
| `Loopback` | no entry (unsupported) | unchanged | darwin: `Unknown`, proved by the canary; linux: `Unsupported` with the reason above (unless task 7 lands); windows: `Unsupported` |
| `Background` | no entry (unsupported) | unchanged | darwin, linux: `Composed`, "the workbench's commands run at nice 10"; windows: `Unsupported` as for every engine |
| `RestrictTools` | `Composed` | reason gains "and the library's workbench tools" | same |

The Windows rows come from `platform()`, which already exempts API sessions
from the refusal that applies to CLIs. It gains an `OpenAICompatible` case for
`Sandbox` and `Loopback`. The doc comment on `Sandbox` is widened to "native
tools, or the library's command tool, inside a proven OS sandbox". The new
features have no entries for the CLIs, whose own tools already do this, so they
fall through to "not offered by this engine". crew-assistant decides:

- that a session may hold a read-only role from `WorkspaceRead`;
- that it may hold an editing role from `WorkspaceWrite`;
- that it may hold an implementer or QA role from `Sandbox`, which is `Usable`
  only where the platform can prove it.

## OpenRouter

OpenRouter is used through the existing OpenAI-compatible transport, not as a
new engine:

- **Endpoint:** `BaseURL: "https://openrouter.ai/api/v1"`, `Dialect:
  OpenAIChatCompletions`. `API.Endpoint` derives
  `https://openrouter.ai/api/v1/chat/completions` and `.../models` from it.
- **Credential:** the caller's key, sent as `Authorization: Bearer`.
- **Effort:** `EffortParameter: EffortReasoningObject`, sent as `reasoning:
  {"effort": …}`.

Its own gaps are these:

1. **An error inside a 200 response.** OpenRouter documents that an error can
   arrive after the request has started processing, with an `error` object
   whose `code` is a number, such as 429 or 502. Main already classifies such
   an object (`apihttp.EmbeddedFailure`, used by `parseChatCompletion` and, for
   a failure reported mid-stream, by the streaming path through the same
   parser). It does so deliberately by allowlisted *string* codes only. Every
   other object becomes `provider_error`, not retryable, "because a numeric
   code there is not an HTTP status". That rule stays the default for every
   endpoint. So today a free model's embedded 429 is a non-retryable
   `provider_error`, and the caller cannot tell to back off.

   The change is scoped to the typed OpenRouter option in (6), because that
   option is the caller's explicit statement that the endpoint follows
   OpenRouter's documented error shape. When `API.OpenRouter` is set, an
   embedded object whose `code` is a JSON integer is classified by the same
   table as `StatusFailure`, with that integer as the status. An embedded 429
   is then `rate_limited` and retryable, 402 is as in (2), and 502 and 503 are
   as in the status table. Only the error object is available, so `RetryAfter`
   stays zero. Everything else about main's rule holds:
   - string codes are still consulted first;
   - usage stays reported next to the error;
   - the message text is never read;
   - without the option, or for a non-integer code, the result is
     `provider_error` as today.

   A test covers both the non-streaming and the streamed path, so they cannot
   diverge. The condition for building it was confirmation of the shape. The
   errors page, re-read on 2026-09-30, now documents both 200-status shapes
   with integer codes (a whole response with only `error`, and a streamed
   chunk carrying `error` beside `finish_reason: "error"`), so it was built
   (task 1); the live check that they occur is still pending. A float, a
   digit string or a code outside 400–599 stays `provider_error`.
2. **402.** Today it is an untyped `http_402`. It becomes `insufficient_credits`
   with `CauseQuotaExhausted`, which main added for a used-up prepaid balance,
   and it is not retryable, because waiting does not fix it. This follows the
   precedent of `insufficient_quota`. A 402 means the same to every
   OpenAI-compatible endpoint, so this mapping applies to all of them, not only
   under the OpenRouter option. OpenRouter documents 402 even for free models
   when the account's balance is negative. **Known divergence:** its errors
   page now also documents a 402 with `Retry-After` when
   `metadata.limit_source` is `openrouter_in_flight_budget`, which would clear
   by waiting. The library still types every 402 as not retryable with no
   `RetryAfter`, because metadata is never read; telling that case apart is a
   possible follow-up with the `X-RateLimit-*` work below.
3. **Rate-limited free models.** `:free` variants have per-minute and per-day
   request limits. A 429 status already maps to `CauseRateLimited`, retryable,
   with a `RetryAfter` of at most one hour taken from a delta-seconds
   `Retry-After`. No code change is needed for that path, and (1) covers the
   embedded one. The library does not retry or back off.
   The caller reads `harness.ErrorFacts` and waits (`RetryAfter`, or its own
   policy when that is zero). Across a composed session this reaches the caller
   as a `TurnError` through `apiTurnFailure`. The daily limit is also a 429, so
   the caller may see it again after waiting a short time. Whether OpenRouter
   tells the two limits apart in headers needs a live check. Reading
   `X-RateLimit-*` headers or `metadata` (`error_type`, `limit_source`) to
   tell them apart, or to fill `ResetsAt`, is a possible follow-up; task 1
   does not.
4. **Models without tool calling.** A workbench, like every API session, sends
   `tools`, so a model without tool support cannot take part:
   - **Detected:** `catalog.Model` gains `Parameters []string` (with
     `ParametersKnown`, like `EffortsKnown`). It is read from each entry's
     `supported_parameters` only as a JSON array of at most 128 non-empty
     strings of at most 64 bytes each, repeats dropped and order kept; `[]`
     is known and empty. Null, a missing field, another type or anything over
     the bound makes that entry's parameters unknown, and the rest of the
     catalog is still returned. Each parameter is in the credential-echo
     check. CLI engines leave both unset (unknown). `parseAPIModels` also
     keeps reading `context_length`. Pricing is not parsed in this work.
   - **Refused:** `catalog.SupportsTools(model)` reports `true`, `false` or
     unknown. Configuring a session never triggers discovery (the API
     transports design), so the caller passes the entry it chose the model
     from as `session.Options.CatalogModel`. `normalizeAPI` checks it right
     after `model_required`: an entry whose `ID` (or non-empty `Resolved`) is
     not `Model` is `conflicting_options`; one that is known and lacks `tools`
     is refused with `RefusedModelWithoutTools`
     (`model_without_tool_calling`, a capability failure) before the runtime
     home, the transcript lock, any file or any request. No entry, or one
     with `ParametersKnown` false (including an entry cached before this
     field existed), is unknown and the session goes ahead: a failed or
     missing discovery never blocks an explicitly configured model. The
     entry is read only during normalization, never kept and not in the
     `Ref` digest. A CLI engine refuses it with `conflicting_options`.
     Because the check runs in `normalizeAPI`, it covers stage 1's
     workbench too, ahead of the workbench's own checks.
   - **At request time:** with the typed routing option below,
     `require_parameters: true` asks OpenRouter to route only to providers that
     honour every parameter sent, `tools` included. What OpenRouter answers when
     no such provider exists needs a live check. It is expected to be a 404 with
     a numeric code. It stays `http_404` unless the live check finds a
     structured, non-prose field to type it by.
5. **Model listing.** `GET https://openrouter.ai/api/v1/models` returns one
   unpaginated `data` array, which `listAPI` already reads, bounded at 4 MiB.
   Whether the list needs authentication, and whether its size fits the bound,
   need a live check. If it is public, the caller's credential is still sent,
   because the transport has no unauthenticated mode for non-loopback URLs, and
   that is not worth adding.
6. **A typed routing option.** `harness.API` gains
   `OpenRouter *OpenRouterRouting{RequireParameters bool; DataCollection
   string}`, sent as `provider: {"require_parameters": …, "data_collection":
   "allow"|"deny"}`. It is refused unless the value is valid. It is never
   inferred from the host name. It is not a pass-through: it has one field per
   documented key, each with a test. Zero data retention (`zdr`) and provider
   order are left until a caller needs them. It is a request option, not a
   capability, so `harness.Support` is unchanged. It is left out of a
   session's `Ref` digest like `EffortParameter` and `Streaming`: resuming
   under changed routing continues the same conversation. A session keeps its
   own copy, so the caller changing theirs later does not reach a running loop.
7. **The data-policy caveat.** Prompts and every tool result, including file
   contents the workbench reads, go to OpenRouter and on to the provider that
   serves the model. Many free endpoints are served by providers that may log or
   train on inputs, and OpenRouter's account privacy settings decide whether
   those endpoints can be used at all. `DataCollection: "deny"` restricts
   routing to providers that do not collect data, and may leave a free model
   with no endpoint. The README says this beside the workbench, and
   crew-assistant shows it when a role is given an OpenRouter model.

Out of scope: attribution headers (`HTTP-Referer`, `X-Title`) other than as a
typed option with a test, plugins such as web search, streamed text deltas to
the session's caller (`API.Streaming` itself has landed and needs nothing from
this design beyond the shared classification in (1)), the Responses
dialect, `/api/v1/key` and `/api/v1/credits` as an `Account` implementation
(noted as a possible later way to report credit usage), and pricing.

### What was checked, and what needs a live check

**Relied on from OpenRouter's documentation.** These pages were not re-fetched
while writing this document, because the environment had no network. They
were re-read on 2026-09-30 while planning task 1, which found the list still
right, with two additions to the errors page: it documents both 200-status
error shapes with integer codes (see (1)), and a 402 with `Retry-After` for
`limit_source: openrouter_in_flight_budget` (the divergence in (2)). The
implementation environment had no network, so the pages were not re-fetched
again there.

- API reference (`/docs/api-reference/overview`): the Chat Completions request
  and response shape, `tools`/`tool_calls` in OpenAI form, `usage`, Bearer
  authentication, and the base URL `https://openrouter.ai/api/v1`.
- Errors (`/docs/api-reference/errors`): the `{"error": {"code": <number>,
  "message", "metadata"}}` shape; 400, 401, 402 (insufficient credits), 403
  (moderation), 408, 429, 502 (the upstream model failed) and 503 (no provider
  meets the routing requirements); and that errors after processing began can
  arrive with a 200 status.
- Rate limits (`/docs/api-reference/limits`): per-minute and per-day request
  limits for `:free` models, the higher daily limit after buying credits, and
  402 on a negative balance.
- Models API (`/docs/api-reference/list-available-models`): the `data` array
  with `id`, `name`, `description`, `context_length`, `pricing`,
  `supported_parameters`, and a `supported_parameters` query filter.
- Tool calling (`/docs/features/tool-calling`): OpenAI-shaped tools, and that
  only some models support them.
- Provider routing (`/docs/features/provider-routing`): the `provider` object
  with `require_parameters`, `data_collection`, `order`, `allow_fallbacks` and
  `zdr`.
- Privacy and logging (`/docs/features/privacy-and-logging`): prompts are not
  logged by OpenRouter unless the account opts in, providers have their own
  policies, and the account setting controls whether endpoints that may train
  on inputs are used.
- Reasoning tokens (`/docs/use-cases/reasoning-tokens`): `reasoning.effort`.

**Needs a live check with a real key.** This is manual and never run in CI,
per `AGENTS.md`. The owner runs it. **Status: pending.** Task 1 had no key and
no network, so none of these has been run; the code follows the documented
shapes, and the tests use synthetic responses in those shapes:

- the headers on a free model's 429, whether `Retry-After` is present, and
  whether the per-minute and per-day limits can be told apart;
- the status and body when `require_parameters` finds no provider that
  supports tools, and when `data_collection: "deny"` leaves no endpoint;
- that an error inside a 200 response really occurs, in a whole response and
  mid-stream with `API.Streaming`, that its `code` is an integer, and which
  codes appear (429 on a free model, 502);
- whether `GET /api/v1/models` needs authentication, and its current size
  against the 4 MiB bound;
- which `reasoning.effort` values each candidate free model accepts, and what
  an unsupported one returns;
- the shape of `usage` for non-streaming responses: whether `cost` and
  `prompt_tokens_details.cached_tokens` are present without opting in, and
  whether the library should read `cost` into `harness.Cost`;
- that a free model's `tool_calls` pass `parseChatCompletion`'s terminal rules
  (`finish_reason`, unique IDs, JSON-object arguments), for each model the
  owner means to use.

## Ordered implementation tasks

1. **OpenRouter transport fixes.** Re-check the documentation list above and
   run the live checks. Then:
   - add `API.OpenRouter` routing (`require_parameters`, `data_collection`)
     with request-shape tests;
   - under that option only, classify an embedded error object's integer
     `code` by the status table, tested on the whole-response and streamed
     paths, if the live check confirms that shape;
   - type 402 as `insufficient_credits` (`CauseQuotaExhausted`) for every
     endpoint.

   Record the live findings in this document.

   **Done (2026-09-30), except the live checks.** `API.OpenRouter` routing,
   the embedded integer code under that option (the documented shape stood in
   for the live confirmation, see (1)), and 402 as `insufficient_credits` have
   landed with tests on both paths and in a session. The documentation list
   was re-read; the live checks above are still pending for want of a key.
2. **Catalog tool support.** Add `catalog.Model.Parameters`/`ParametersKnown`
   from `supported_parameters`, and `SupportsTools`, with fixtures. Update the
   README's Models section.

   **Done (2026-09-30), except the live checks.** Parameters are recorded
   under the bounds in (4). `session.Options.CatalogModel` carries the
   caller's entry, and `normalizeAPI` refuses a known model without `tools`
   with `RefusedModelWithoutTools` before launch, on `Start`, `Open` and
   `Resume` alike. That also covers the workbench. Unknown goes ahead. The
   tests use fixtures and an in-process model; OpenRouter's real
   `supported_parameters` values are among the live checks above.
3. **Stage 1 (LAH-2).** Add `Options.Workbench` and the `WorkDir` rules, the
   reserved names, `read_file`/`list_files`/`search_files` on `os.Root`, the
   shared file-check package, the `Ref` digest only when set (golden digests
   unchanged, plus a new golden for a workbench), and `WorkspaceRead` in
   `Support`. Also add `workbenchDefinitions` feeding both `apiTools` and
   `newDirectToolHost`, and its tests:
   - a request-shape test that the model is offered the tools;
   - a loop test that calls each tool beside a caller tool.

   Also add one opened-handle check used by every tool: regular file or
   directory, `file_not_regular`; one link, `file_linked`; same mount,
   `file_other_mount`, through statx or fdinfo, `st_dev`, or the volume serial
   and final path. Add the non-blocking `O_NONBLOCK|O_NOCTTY` opens, the
   refusal of Windows reserved names, and the FIFO, socket, mount and swap
   tests, including the Linux CI bind-mount step. Add the tracked workspace
   worker with its bounded wait and `workspace_io_stuck`, and the
   stalled-read tests, including 100 repeated cancellations. Add the disjoint
   `WorkDir`/`RuntimeHome` check and its tests. Add the reserved
   `.harness-workbench-*.tmp` pattern hidden from the read tools, and the
   hard-link and swap concurrency tests, gated through `internal/testenv`.
   Update the README.
4. **Stage 2a.** Add `write_file` and `edit_file` with atomic replacement in
   the specified order (private `O_EXCL` temporary, file and directory sync).
   Clean up this session's temporaries on the error path before the rename,
   and at `Open`/`Resume`/`Close`. Classify failures on either side of the
   rename, with `write_outcome_unknown` recorded as `outcome: "unknown"` and
   never rolled back or re-run. Add the fault-injection layer with the
   before-rename, rename and after-rename (directory sync) cases, and the
   old-or-new crash test.
   Add the symlink-free handle walk (`file_is_symlink`,
   `path_through_symlink`, and `.git` found by identity), `NewFileMode` with
   the deterministic final mode, the `file_read_only` refusal, and their tests,
   including the re-executed umask cases. Add the effective `NewFileMode`
   (0600 when unset) to the `Ref` digest when `Write` is set, with the
   0600-then-0644 resume-mismatch test, the unset-equals-0600 test, the
   read-only-digest-ignores-the-mode test and the `Write`-workbench golden.
   Also add the `.git` refusal and escape
   tests, their request-shape case, `WorkspaceWrite`, and the README.
5. **Stage 2b (LAH-3), macOS.** Add the Seatbelt profile generator, the runner
   (environment, scratch, timeout, bounds, `WaitDelay`, containment, sweep at
   `Close`), the canary with its no-sandbox and widened-rule tests, the cache
   key, and the shared `layout` whose stand-ins lie outside the read set. Add
   the pinned macOS system read set under `(deny default)`, with metadata-only
   ancestors. Add the canary's `home`, `outside`, `system`, `readset`, `link`,
   `nested` and `runtime` checks, the refusal of a `RuntimeHome` inside the read
   set, and `Commands.Read` refused where it overlaps `RuntimeHome`. Add the
   refusal of a `WorkDir` that contains home, and the profile's refusal of mount
   operations with the canary's `mount` check, `run_command`'s request-shape case, `Sandbox`/`Loopback` for darwin,
   and a session test that edits and runs a command.
6. **Stage 2c (LAH-3), Linux.** Add `bwrapArgs(layout)` with its five phases:
   - every tmpfs;
   - every directory, system destinations included;
   - the pinned Linux system set;
   - shallowest-first binds;
   - the `.git` overlay.

   Add the pinned privilege controls (`--unshare-user --disable-userns
   --cap-drop ALL`, plus the namespace flags), and the refusal of workspace
   paths inside the system set. Add the arguments-simulation unit tests for
   every layout case, and the canary under bwrap at the session's covering
   location and depth, with its `privilege`, `overlay`, `gitmove`, `gitlink`
   and `githardlink` checks and the byte-for-byte `.git` check. Also add
   `sandbox_tool_missing`, `sandbox_tool_outdated` (below 0.8.0) and
   `sandbox_namespaces_unavailable` with their messages, `Sandbox` for linux,
   `Loopback` Unsupported with its reason, and Windows refusal tests. Add CI
   jobs:
   - one that installs bubblewrap and runs the real canary four times
     (`RuntimeHome` inside and outside `$HOME`, `WorkDir` inside `$HOME` and
     under `/tmp`), on the distribution's bwrap and on bwrap 0.8.0 built from
     source;
   - one that runs the adversarial raw-syscall helper test on both.
7. **Linux shared loopback (optional).** Design and prove a session-long
   namespace with a two-command canary, then promote `Loopback` on Linux. Do
   this only if a consumer needs dev servers across commands on Linux.
8. **Consumers.** Update crew-assistant to use `WorkspaceRead`,
   `WorkspaceWrite` and `Sandbox` for role eligibility, `SupportsTools` when a
   model is chosen, back-off on `rate_limited` using `RetryAfter`, and the
   data-policy notice. crew-code-review needs no change unless it adopts API
   sessions. Release each stage as a minor version, as breaking where options
   change.
