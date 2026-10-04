# Workspace storage and content admission (LAH-27)

Status: design draft for review, not implementation or enforcement evidence.
LAH-32 landed as `835527e`. Its refusal, tests, public contracts and capability
claims stay unchanged. LAH-28 implements only after review and consumer demand:
a consumer configures an API-engine role needing Read/Search/Edit, or the owner
requests implementation. No current team uses API-engine roles.

## Decision and scope

Per the owner, WorkDir must be the root of its own per-task local filesystem:
an APFS disk image on macOS, or a dedicated mount on Linux. Ordinary directories
remain refused. There is no Linux-only mode based on undocumented kernel lock
ordering. A subtree mount is insufficient. The mechanism is a filesystem boundary
plus anchored, checked handles, not additional pathname samples.

This is the prerequisite for LAH-28's standalone Workspace and API-workbench
restoration. Native read allow-lists (LAH-21), loopback ports (LAH-24), command
PATH/settlement (LAH-25/30), permission requests (LAH-22), native confinement
(LAH-31), and the command sandbox's hard-link boundary remain separate. Merged
main's command policies, proofs and diagnostics remain mandatory.

## Threat model and import provenance

Consider model requests, sandboxed commands, concurrent callers and external
same-user processes. Names can be linked, unlinked, renamed or replaced at any
point. The claim is filesystem membership of the opened object, not a stable
snapshot, exclusive writers or proof of the origin of every byte.

The caller provisions fresh task storage and authorizes its initial clone and
later byte imports. Copying/downloads and writes through authorized handles are
caller-placed workspace data. A copying importer can itself race outside-source
replacement: callers must authorize sources and use snapshots/quiescence when
stable provenance matters. The library cannot certify that policy. The retained
outside-source counterexample must never be relabeled an authorized import.
Credentials, RuntimeHome, helper state and images stay outside WorkDir; callers
exclude secrets from imports because content results go to the provider.

Mount administrators, raw-device writers and processes able to inject descriptors
into the library or replace its namespace are trusted infrastructure. Those
powers defeat a userspace proof. Same-user processes without those powers may
mutate workspace bytes; confinement must survive that mutation. Protect images
from accidental modification while mounted. Mode 0600 excludes other users, not
a malicious owner-identity process; no stronger integrity claim is made.

## Research and documented basis

| Mechanism | Decision |
| --- | --- |
| Lstat/open/fstat/SameFile/second Lstat | Rejected as admission proof. LAH-32's real one-link outside descriptor passes modeled namespace observations. Matching samples do not prove confinement. |
| Linux i_rwsem/iterate_dir ordering | Rejected by owner: undocumented implementation ordering, with no equivalent macOS proof. |
| Private copied store | Insufficient alone: the importer has the same source race and needs caller provenance policy. |
| fanotify, Landlock, advisory locks and path sandboxes | Insufficient: notifications/locks do not serialize every writer; subject/path restrictions do not prove inode provenance against external processes. Privileged monitoring is not the chosen portable mechanism. |
| Dedicated local filesystem | Selected: documented cross-filesystem link/rename refusal prevents outside inodes entering through these operations; checked handles reject child mounts and substituted outside descriptors. |

References, not executed enforcement evidence:

- Linux [link(2)](https://man7.org/linux/man-pages/man2/link.2.html) and
  [rename(2)](https://man7.org/linux/man-pages/man2/rename.2.html): EXDEV across
  mounted filesystems, including different mountpoints of the same filesystem.
- [statx(2)](https://man7.org/linux/man-pages/man2/statx.2.html): STATX_MNT_ID,
  support masks and STATX_ATTR_MOUNT_ROOT;
  [proc_pid_mountinfo(5)](https://man7.org/linux/man-pages/man5/proc_pid_mountinfo.5.html):
  mount ID, device, filesystem root, mountpoint and options.
- Apple [link(2)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/link.2.html),
  [rename(2)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/rename.2.html),
  [fstatfs(2)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/statfs.2.html),
  and `man hdiutil`: same-filesystem requirements and handle filesystem facts.
- [tmpfs(5)](https://man7.org/linux/man-pages/man5/tmpfs.5.html),
  [losetup(8)](https://man7.org/linux/man-pages/man8/losetup.8.html),
  [mount(8)](https://man7.org/linux/man-pages/man8/mount.8.html): storage and privileges.

Local Apple link, rename and fstatfs manuals were read for this draft. Network
access was unavailable; Linux references and archived links were not fetched.
LAH-28 must verify applicable installed APIs/semantics and CGO-free x/sys coverage.
No APFS-internals or exact historical kernel-event reproduction is claimed.

## Admission invariant and proof

Retain a root directory handle for the Workspace lifetime. WorkDir resolves to
the root of the entire task filesystem, not a directory on shared storage.
Every traversal/edit directory and content handle must match that retained root
identity before use. Read only the actual checked descriptor. Path samples remain
defensive race checks, never the premise. Caller-supplied handles are not trusted.

All ordinary inodes on fresh task storage are created/imported there. An outside
inode cannot acquire a task name through link/rename because the boundary returns
EXDEV. Unlink/rename cannot transfer an open inode to another filesystem. A later
replacement opens either another task inode or a foreign-mount inode; the latter
fails before bytes. A retained task handle stays admitted after its name vanishes.
Link count one is unnecessary to this argument; linked-file refusal stays as
defence in depth. External byte mutation remains permitted. Search can see multiple
versions and edit is atomic replacement, not compare-and-swap against outsiders.

### Linux proof

Statx the root fd with AT_EMPTY_PATH. Require supported STATX_MNT_ID and
supported/set STATX_ATTR_MOUNT_ROOT: inspect both attribute mask and value.
Join mount ID/device major-minor to one bounded, correctly escaped
`/proc/self/mountinfo` parse in the library's namespace. Require filesystem root
field `/`, matching resolved mountpoint and an allowed local type. A subtree
bind has a different root field and refuses. Root `/` does not exclude a bind
of a whole filesystem: trusted per-task provisioning remains required. Mountinfo
and device numbers cannot prove exclusive ownership or import history.

Initial target types: ext4, xfs, tmpfs, enabled individually only after LAH-28
validation. btrfs waits for subvolume/root review. Overlay, FUSE (including
fuse2fs), NFS, CIFS, 9p and unknown types refuse. Record mount/superblock flags;
nodev/nosuid are recommended, not admission evidence. Read-only storage can prove
reads but edits retain read-only refusal. Existing fdinfo fallback can identify
a content handle, not replace missing mount-root evidence. Compare mount IDs,
not st_dev alone, including every parent/temp handle: this rejects same-device
bind-ins. Retaining the root mount avoids trusting a recycled bare mount ID.

### macOS proof

Fstatfs/fstat the root handle. Require f_mntonname equal to resolved WorkDir,
MNT_LOCAL, allowed type, and root-directory identity tied to that opened handle.
Pin f_fsid together with st_dev and compare like fields: fsid is not numerically
st_dev. An unchecked separate pathname lookup is insufficient. Initial target
is APFS images. HFS requires separate validation; nullfs, macFUSE, network and
unknown types refuse. Record flags/owner. Every content and directory handle
must match device and fsid. Another APFS volume in the container is foreign.
Existing st_dev-only checks are not the full new root proof.

### Re-proof, cancellation and lifecycle

OpenWorkspace and every API Start/Open/Resume build fresh metadata-only evidence
before tool advertisement/admission or session state. Never restore proof from
transcripts/helper records or cache it across processes. Bound mountinfo bytes
and checkpoint around syscalls. Cancelled proof closes partial handles and never
publishes success. Syscalls are not instantly cancellable; retain stuck-worker
containment and actual settlement before authorizing replacement work.

Unproved storage normally keeps LAH-32's reduced session surface. Cancellation
of Start/Open/Resume returns cancellation, without inference. Optional
RequireProvedStorage refuses unproved launch before transcript, inference or
command preparation. An unattached mountpoint is an ordinary directory; this
option prevents accidentally running commands in it. Attach before resume.

Replacement of the path after proof cannot retarget the pinned root: use it
until Close. New handles must match that root. Detached live handles retain
the original filesystem; EIO maps to file_unreadable and ENOENT to file_not_found.
A new Open/Resume proves the current mount. For edits pin/check every parent
and temporary before I/O/rename; no multi-component reopen may replace anchors.
Within-volume directory rename remains confined even if the visible path changes.

Close/CancelTools pause admission, cancel work and await actual settlement before
detach or another turn. Keep the serialized worker. After committed rename,
directory sync settles despite cancellation; write_outcome_unknown never means
rollback. Preserve partial search counts and never replay historical calls.

## Operations and typed refusals (LAH-28 specification)

Read, direct/recursive Search and opt-in Edit work only with proved storage.
Retain limits, UTF-8 rules, .git exclusion, symlink/link refusals, nonblocking
regular opens and atomic edit durability. Ordinary directories remain refused.
List, opt-in Write and commands retain current authorization/proof requirements;
Windows is unchanged.

Introduce `workspace_storage_unproved`, capability refusal family, with fixed
sanitized causes: `not_filesystem_root`, `subtree_mount`,
`filesystem_type_unverified`, `identity_unavailable`, `mountinfo_unavailable`,
`not_local`, `root_identity_mismatch`, `proof_cancelled`. No raw mountinfo,
host paths, provider text or diagnostics enter structural errors. Standalone
operations return Result plus *sandbox.RefusalError before argument parsing,
target opens, bytes, matching, parents or temp creation. Missing evidence never
admits content. Wrong-mount handles on proved storage return file_other_mount
with zero bytes; recursive search skips/counts them as today. Strict launch
translates the same facts through session's typed refusal adapter.

Forced absent/reserved hosted calls retain not_offered and never reach handlers.
Per-tool availability conveys the storage verdict. A refused finish/edit must
not latch closing admission. No public/runtime bypass is added.

## Per-task provisioning and estimated cost

Consumers provide fresh storage, stable absolute WorkDir at its root, private
StateDir outside WorkDir/RuntimeHome, TaskID, capacity, trusted importer and
lifecycle owner. Linux needs a caller mounter/privilege policy. Clone into the
mounted root, not a subdirectory; account for ext4 lost+found without deleting
filesystem housekeeping. If Git clone requires an empty destination, initialize
and fetch into the root instead of cloning into a child. Persist volume records
separately from session Ref.
Exclude outside Git alternates and local-object hard links (`--no-hardlinks`).
Cross-volume APFS clonefile sharing is unavailable: each task pays for full Git
objects and working tree. Historical ordinary clones never opt in automatically.

All numbers below are **estimates, not measurements**. Let C be allocated bytes
of an independent clone including .git, F file count, V capacity, N task count.
Example: C=500 MiB, V=4 GiB. Actual crew-assistant clone size was unavailable.
Clone/import time is additional and depends on C, F, storage and Git processing.

| Backend | Estimated setup excluding clone | Example allocation | Privilege/durability |
| --- | --- | --- | --- |
| APFS sparse image | Create about 1–2 s, attach 0.2–1 s; budget 1–5 s cold | C plus metadata; budget tens of MiB, measure empty allocation; grows toward V | User hdiutil normally needs no root for own image; host policy may deny it. Durable. |
| Linux tmpfs | Mount about 1–20 ms | C plus inode/dentry overhead in RAM/swap; limit is not upfront allocation | CAP_SYS_ADMIN in hosting namespace or broker. Lost on reboot; unsuitable for durable clones without export/reimport. |
| Sparse loop-ext4 | Format 0.1–1 s for a few GiB with lazy init; mount 10–200 ms | C plus roughly 1–2% of V metadata and journal, often tens/hundreds of MiB; sparse allocation varies | Root/CAP_SYS_ADMIN and loop access or configured udisks2/polkit. Durable. |

Private user-namespace mounts work only if the library/task occupy that namespace;
they are invisible to a daemon outside it. Initial helper uses a caller broker.
Tmpfs can exhaust RAM/swap and images can exhaust backing storage before V;
ordinary typed I/O failures apply. Ten 500 MiB tasks need at least 5 GiB plus
per-volume overhead, without sharing.

Measurement procedure for later provisioning validation: disposable secret-free
clone; record OS/filesystem/tools, allocated/apparent C with du, F, V and N.
Time create/format, mount, import, sync and normal detach separately over 20
cold/warm trials, reporting median/p95. macOS: hdiutil create -type SPARSE -fs
APFS -size <cap>, attach -nobrowse -noautoopen -owners on -mountpoint <WorkDir>;
measure allocated image bytes empty/after clone, not apparent capacity. Linux:
compare truncate/mkfs.ext4/loop mount and tmpfs through broker; record lazy-init,
journal, free-space/image-du/tmpfs/swap deltas. Include crash/reattach timing.
Delete only confirmed unmounted disposable resources. No mount experiment ran
here; these estimates must not become measured capability evidence.

## Helper sketch (not implemented)

Possible package workspacefs: Create(ctx, Spec{WorkDir, StateDir, TaskID,
Capacity, Backend, Mounter}), Attach(ctx, Record), Detach(ctx, Record),
Reap(ctx, StateDir). Consumer owns authorization, retention, imports and retries.
Linux never escalates implicitly: caller Mounter may use preauthorized sudo -n
or udisks. APFS uses structured hdiutil plist output. No passwords or shell
interpolation; records contain no credentials.

Private anchored no-symlink StateDir, exclusive record creation and per-volume
lifecycle lock. Journal intent before create/attach and completion after each
step, with atomic replacement/directory sync. Record schema, random task/volume
token, backend, image identity/path, mountpoint/device, phase, owner PID/start
identity. Recording intent before creation covers crash-before-attach. Cancel
after side effects records partial state, never presumed rollback. Attach is
idempotent only after matching actual device/image/mountpoint; conflicts refuse.

Detach waits for sessions/workers/commands to settle and handles to close.
Bound normal unmount/detach; busy/cancelled/uncertain teardown preserves record
and image. Never automatically force-detach, lazy-unmount or delete after timeout:
live writers and resumable data may remain. Separate explicitly authorized caller
maintenance can decide otherwise. LO_FLAGS_AUTOCLEAR aids loop cleanup but does
not prove mounts/users have disappeared.

Reap holds the lock and correlates records with hdiutil info -plist or
mountinfo/loop backing identity. Free lock alone does not prove dead ownership.
Establish PID/start identity, account for surviving process trees and resumable
tasks; uncertainty preserves resources. Dead launcher does not mean disposable
task. Only after confirmed unmount and retention approval remove the matching
image beneath StateDir, never follow replacements or delete foreign images.
Crashes at every journal phase permit conservative idempotent recovery. Helper
claims never replace independent OpenWorkspace proof after attachment.

## Public contract, resume, release and consumers

LAH-28 adds an immutable per-Workspace storage verdict and defensive per-instance
availability/definitions. Pathless ToolAvailability/Definitions cannot grant
content; replace/deprecate them depending on whether v0.24.0 has been published.
Verify publication through release/module-proxy evidence then; this offline draft
does not establish that fact.

At restoration harness.Support aggregate WorkspaceRead/WorkspaceWrite is Unknown
on Linux/macOS with the dedicated-root condition: Support has no WorkDir. Proved
instances report composed tools; unproved instances report absence/fixed reasons.
Keep current Unsupported until enforcement is validated. Advertisement and
admission share one verdict. Replace session/api.go's hard-coded read_file notice
check with effective verdict-derived tools/notices.

Optional RequireProvedStorage on Workspace/session narrows launch only and stays
outside workbenchDigest. Preserve WorkDir, Write, Commands, Loopback, Read and
effective NewFileMode digest fields and Ref format. Never persist filesystem IDs
as resume authority: reattach assigns fresh IDs, so re-prove. At the same path,
resume can gain/lose tools; regenerate notices without stale accumulation or
historical replay. Moving WorkDir needs explicit migration/new session, never
silent reference rewriting.

Storage proof stays separate from native/command proof caches. No cross-process
storage cache. If volume integration changes command launch configuration,
LAH-28 must update command proof identities and re-prove, never accept old
evidence. This design changes no command keys or generated assets.
Restoration ships as a breaking minor with README/design/release migration notes
and both consumer updates/validation against published dependencies, no committed
local replaces. LAH-33/34 and CA-115's disabled-tools adoption remain valid.

crew-assistant opt-in: backend/capacity/Linux-mounter settings; create/attach
before clone, record volume in task state, attach before Start/Open/Resume,
RequireProvedStorage for roles needing content, show per-instance verdict for
eligibility. Preserve stable WorkDir in Ref. Settle then detach after landing,
abandonment or retention decision; conservative startup reap retains resumable
tasks. Native roles/historical ordinary directories keep current contracts.
crew-code-review needs equivalent lifecycle only when it adopts API file tools.
No provisioning burden is introduced now: implementation waits for the trigger.

## Evidence, obligations and LAH-28 deterministic regressions

LAH-26 is stopped investigation, not landed policy. Supplied CI run 37167992825,
commit 3017616, bubblewrap 0.8.0 returned OUTSIDE-MARKER-4d1c: an obligation for
all-interleavings confinement, not identification of the exact kernel race.
LAH-32 supersedes whole-session refusal with continuing reduced sessions.

Its modeledLinkWorkspace opens a real workspace hard link, unlinks only that
name, preserves outside source and descriptor with real identity, same-mount,
regular-file and Links=1 facts. Injected namespace observations let retained
openRegular admit it; TestWorkbenchModeledReadAdmissionBaseline reads the marker.
Completed-unlink and ordinary-file controls remain. This disproves sampling,
not a forced pause inside kernel unlink or APFS internals. The
[evidence record](2026-10-03-sandbox-package.md#lah-32-draft-6-exact-candidate-owner-results)
separates supplied/local Linux 0.9/macOS checks and accepted post-landing
Linux 0.8/Windows CI; none proves this future mechanism.

| Evidence | Obligation |
| --- | --- |
| Outside source survives Links=1 | Filesystem boundary, explicit imports; never infer provenance from link count. |
| Namespace samples all agree | Anchor actual handles; missing root/mount evidence refuses. |
| Read/Search/Edit expose content, including match results | One verdict before I/O at public entry points and hosted admission. |
| Continuing sessions and historical content | Preserve reservations, unaffected tools, fresh notices and no replay. |
| Interrupted effects/owners can survive | Settle workers, journal side effects, preserve uncertain outcomes/resources. |

LAH-28 tests use instance-local seams and channel handshakes, never sleeps or a
public bypass; real syscall controls are distinct from modeled interleavings:

1. Ordinary directory with retained outside fixture refuses storage_unproved
   before statName/openName/contentStep; zero bytes and marker absent. Keep the
   baseline beneath refusal as historical counterexample.
2. Real Linux tmpfs/loop-ext4 and macOS APFS: outside os.Link/os.Rename into root
   fail EXDEV with source unchanged. openName injection of outside descriptor
   yields file_other_mount before bytes for Read, both Search forms and Edit.
3. At least 200 handshake repetitions: unlink after open, within-volume rename
   out of requested subtree, replacement rename, foreign mount substitution,
   Linux same-device subtree bind-in, root replacement and detached-root lifetime.
   Retained task bytes remain admitted; rename across the filesystem fails EXDEV.
   Parent/temp checks prevent foreign-mount edit writes.
4. Proof refusals: mountinfo root not '/', unreadable/truncated/malformed proc,
   unsupported mount-root attribute, identity failure, overlay/FUSE/NFS/nullfs,
   macOS mountpoint mismatch/nonlocal, cancellation at every boundary. Fixed
   facts, zero content I/O. Whole-root bind is never proof of fresh provenance.
5. Positive dedicated-filesystem Read/direct-recursive Search/Edit controls,
   bounds/.git/symlink/link/atomic durability. Start/Open/Resume advertise all
   configured proved tools without notice; same-path reattach flips availability
   both ways without replay. Strict mode refuses unattached roots before state,
   inference/commands. Forced calls to absent tools never reach handlers.
6. Cancellation/Close/serialization require actual settlement before replacement;
   refused handler never closes admission; partial search counts and edit
   before/at/after-rename old-or-new/unknown outcomes retain their contract.
7. Linux fake Mounter and disposable macOS image helper tests: crash at each
   journal step, PID reuse, surviving jobs, busy detach, replacement records/
   images, conflicts, idempotent attach, reattach/resume and conservative reap.
8. Record tested commit, Go/OS/kernel/filesystem/sandbox versions, commands,
   outcomes/skips, host versus contained. Actual Linux bubblewrap 0.8/0.9 and
   macOS enforcement belong to LAH-28. Validate every enabled filesystem type
   or leave unverified. Preserve Windows runtime behavior and full vet/race CI.

LAH-27 runs unchanged go vet ./..., go test -race ./..., Windows test
cross-compilation and project run_check. Retain TestContentToolsRefuseBeforeIO,
TestWorkbenchHardLinkAndConcurrentSwaps, TestWorkbenchModeledReadAdmissionBaseline,
TestWorkbenchDisabledContentSessionLifecycle and TestWorkbenchHistoricalContentRecovery.
No implementation-shaped tests or runtime changes belong to this patch.

Review must assess the invariant, permitted imports, trusted mount authority,
root/per-handle identity and conservative recovery. Until review passes this is
a draft, not authority to restore tools. A partial stop leaves draft docs only;
no refusal is relaxed.

## Design-patch validation

Writing checkout incorporates main `f5eab45`, supplied by the task runner.
No Go, dependency, workflow or generated files were changed.
Local Go 1.27.1 darwin/arm64 executions:

| Check | Result |
| --- | --- |
| `go vet ./...` | Exit 0 |
| `go test -race ./... -timeout 180s` | Exit 0; ordinary environment probes may skip, not future storage enforcement evidence |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -exec=true ./...` | Exit 0; compilation only, not Windows runtime |
| daemon `run_check` | Exit 0, timed_out=false, empty stderr; platform/skip provenance unspecified |

The CI matrix remains unchanged. These results do not claim fresh runtime
execution on every CI platform or validation of the proposed volume mechanism.
Design review and LAH-28 enforcement evidence remain separate gates.
