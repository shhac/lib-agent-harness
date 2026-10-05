# Command process inspection

ProcessInspection is an opt-in shared command requirement. Linux uses the
existing per-command bubblewrap PID namespace and private proc mount. Separate
Run/Start trees, including commands of one Open, are outside the boundary.
Native engines/runs and Windows remain Unsupported. PATH/read, execution,
network, priority and file-tool restrictions remain unchanged.

macOS is Unsupported with process_inspection_unenforceable: no received evidence
proves Seatbelt can confine all relevant process-info/sysctl interfaces to one
sandbox instance. Reports of kern.procargs2 bypasses on macOS 15/26 remain
hypotheses about this profile. No OS version receives an inspection grant.

## Proof, identity and admission

Before command state exists, Linux runs a disposable inspection trial with the
same launch arguments and read layout. A contained outside shell retains a
random marker in argv/environment and holds a marker-named open file. A pipe
ping/pong establishes live fixture responsiveness before the negative trial. Host
reads prove only this fixture's positive availability. Inside, nice starts a
live sleep child. Installed ps is compared to /proc/stat for PID, PGID, nice,
state and birth ticks, with expected inherited PGID and nice five above the
launch priority, capped at 19 (15 with Background). ps lstart must parse and
command must be present. The child pauses while the launcher walks only its
controlled bwrap subtree, maps PID/PGID through NSpid/NSpgid, and compares host
nice and birth ticks with the in-namespace observations. The trial compares PID
namespaces, enumerates private /proc and attempts outside cmdline, environ,
descriptor paths and ps. Marker exposure refuses admission.

The shell PID namespace must differ from the outside fixture namespace, and
its proc status must report one NSpid ID (the rooted check). Together these
controls establish that proc was mounted for this namespace; every visible entry
is therefore a member or descendant. Missing host PID means namespace
invisibility; a visible numeric collision belongs to this private proc view.
Non-rooted per-entry NSpid checks only establish readable, well-formed status:
the first ID always equals the directory PID in real procfs. They are sanity
checks, not independent membership or permission-refusal proofs. Missing status
refuses admission as unavailable; it does not establish a host visibility breach. Missing positive metadata reports
process_inspection_unavailable; escaped boundaries report sandbox_not_enforced;
cancellation without observed marker exposure reports probe_timed_out. A marker
already observed in partial output wins over cancellation and reports
sandbox_not_enforced. All carry the fixed process_inspection
proof step, including hosted translation. Ordinary exit/cancellation kills and
reaps the fixture tree. Abrupt death leaves bounded synthetic fixtures; existing
--die-with-parent and recovery mechanisms remain in force.

Inspection adds bwrap-workbench-v5-process-inspection-v3 to proof keys. Legacy
keys cannot certify it; non-inspection keys are unchanged. Linux still proves
each Open. Hosted digests wrap the new power only when true, preserving legacy
references when false. No durable verification evidence is introduced.

## Research and evidence

TestProcessInspectionResearch is an opt-in observational macOS matrix: baseline,
explicit denial, same-sandbox candidate and self candidate. Targets include an
own child, controlled unsandboxed same-user shell and separate instances with
identical and different profiles. Interfaces include ps metadata/environment/all
listing, lsof, kern.proc.pid/all/pgrp, kern.procargs2 and CGO-free proc_info.
Profile parse failure is an observation, never proof. Production remains refused.

### Received evidence: source commit 5f70c8c

Linux: Ubuntu 24.04 VM, aarch64, kernel
`7.0.14-orbstack-00380-ga7e0a2dc9535 #1 SMP PREEMPT Fri Aug 7 03:48:40 UTC 2026`,
bubblewrap 0.9.0. Commands: `uname -a`, `bwrap --version`, and
`AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -v ./sandbox ./session . -run ProcessInspection`.
Session and root passed. Sandbox failed TestProcessInspectionCanaryReal
(background=false) and TestProcessInspectionSeparateCommandsReal with
sandbox_not_enforced at process_inspection. The first failure stopped the
background loop; background=true was not established. CancelledLiveChildReal
and all synthetic inspection tests passed. No transcript was logged, so the
exact triggering control cannot be recovered. The old judge matched control
words in its own shell argv; enumeration also treated ptrace-denied PID 1
namespace links as foreign processes. Both paths are corrected without changing
permissions. This failed run proves no inspection capability.

macOS: 27.0, build 26A428, arm64, sandbox-exec SHA-256
`58839ef01b4eef8aac0d2aa8f9d1c074ae45aafe3533965b030672450064acc8`.
Commands: `sw_vers`, `shasum -a 256 /usr/bin/sandbox-exec`, and
`AGENT_HARNESS_INSPECTION_RESEARCH=1 AGENT_HARNESS_TEST_NO_SKIP=1 go test -v ./sandbox -run '^TestProcessInspectionResearch$'`.
Research reported PASS (2.07s), but was inconclusive: ps and the helper binary
were denied execution (126), so ps/sysctl/proc_info boundaries were not measured.
lsof exited 0, showing bash PIDs 83846/83848, executable paths /bin/bash and
/usr/lib/dyld, and fd 0–2 as /dev/null; cwd was denied and fd 3 gave no more
information. The excerpt does not map these PIDs to target labels. This potential
baseline observation belongs to LAH-46; it confirms neither an argv/environment
leak nor confinement. macOS remains Unsupported. Research now grants literal
tool read/exec access only in disposable profiles, labels targets and reports
execution denials as inconclusive failures, never process-info refusals.

### Received evidence: source commit 73590fb57337b859ee015cc5161110a950d48f38

Ubuntu 24.04 VM, aarch64, kernel
`7.0.14-orbstack-00380-ga7e0a2dc9535 #1 SMP PREEMPT Fri Aug 7 03:48:40 UTC 2026`,
bubblewrap 0.9.0. Exact commands supplied for this run:

```sh
git rev-parse HEAD
uname -a
bwrap --version
AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -count=1 -v ./sandbox ./session . -run ProcessInspection
```

TestProcessInspectionCanaryReal passed both background=false and background=true.
This is the first positive Linux canary evidence: live child metadata and the
outside-fixture negative controls passed on the installed kernel and sandbox.
CancelledLiveChildReal, all synthetic inspection tests (including judgment-leak
and outer cancellation translation), session and root passed.

TestProcessInspectionSeparateCommandsReal failed at the second Open with
`runtime_home: the runtime home must be readable only by its owner`, before its
inspection proof ran (empty diagnostic transcript). The different-RuntimeHome
fixture used t.TempDir without chmod; under the VM's umask its mode was 0755.
Production correctly refused it. The fixture now explicitly uses 0700, matching
commandSandboxOptions. The first same-Open branch completed before this refusal;
the different-Open negative regression and its proc-helper checks remain unproved
until the fixed-revision rerun. The focused command therefore failed overall;
these results do not establish a passing full Linux suite or bubblewrap 0.8.0
support. Linux Support remains Unknown with fresh proof per Open.

### Revision and remaining evidence

The task recorder assigns the corrected candidate's commit. No fingerprint is
used in place of the received source commit.

Local runner: macOS 27.0 build 26A428, arm64, Go 1.27.1. sandbox-exec SHA-256:
`58839ef01b4eef8aac0d2aa8f9d1c074ae45aafe3533965b030672450064acc8`.
The research command with AGENT_HARNESS_INSPECTION_RESEARCH=1 refused nested
sandbox-exec (permission denied, exit 71), through RequireNestedSandbox. No
enforcement assertion ran. `go vet ./...`, `go test -race ./...` and
`CGO_ENABLED=0 go build ./...` passed locally. Linux/amd64 and Windows/amd64
CGO-free builds, vet and package test compilation passed; these are compilation,
not platform execution. One inherited-nice synthetic fixture failed because its
text replacement also changed a sample date; the corrected focused race test
and full race suite passed. Generic checks and skipped canaries supply no
platform inspection proof. The project's run_check result is reported separately
in the task handover because it supplies no OS/sandbox provenance.

Runner commands, with cached dependencies and no inference:

```sh
sw_vers
shasum -a 256 /usr/bin/sandbox-exec
AGENT_HARNESS_INSPECTION_RESEARCH=1 AGENT_HARNESS_TEST_NO_SKIP=1 go test -v ./sandbox -run '^TestProcessInspectionResearch$'
uname -a
bwrap --version
AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -v ./sandbox ./session . -run ProcessInspection
go vet ./...
AGENT_HARNESS_TEST_NO_SKIP=1 go test -race ./...
CGO_ENABLED=0 go build ./...
```

Record source revision, OS/build, sandbox hash/version, exact commands, outcomes
and limitations for every external run. Still owed: corrected macOS 27/26 matrix,
the fixed-revision Ubuntu 24.04/bubblewrap 0.9.0 separate-command regression and
canary rerun (both background subtests), and bubblewrap 0.8.0 enforcement,
strict full suites and Windows runtime CI. The owner declines a do-not-merge PR
and accepts CI when the change lands on main. Unavailable prerequisites may skip by name; enforcement failures
must fail. Baseline macOS leak conclusions require received observations and
belong to the separate follow-up task.

Plan differences: the shell's one-ID NSpid proves proc is rooted in its private
namespace; negative collisions and enumeration use namespace-relative NSpid
as sanity checks instead of ptrace-gated namespace links (including PID 1).
Isolation rests on the rooted proc view and distinct namespace, not these
per-entry checks. Missing membership
observations report unavailable. Standalone emitted control lines are judged,
not control words in shell argv. Real tests log bounded, marker-redacted
transcripts and the triggering control on failure through instance-local seams.
Host child
identity uses NSpid/NSpgid. Research peers are direct sandbox-exec instances,
not hosted Open siblings (production macOS inspection is refused). Linux
regressions exercise kill-0 and pidfd with positive self controls; ENOSYS is
recorded as unavailable pidfd, never proof of isolation. Instance-local
cancellation seams cover fixture admission, launch, inside execution and
judgment with no command state and a reaped fixture. A real Linux cancellation
regression cancels after the live child announces readiness; bwrap status and
EOF on the pipe inherited by sleep must settle. Abrupt-death coverage is provided
by existing command containment/recovery tests, not a new inspection-specific
crash test. These differences and missing external observations must not be
represented as completed external enforcement evidence.
Consumer adoption and priority repair remain separate.

Research marker measurements: the helper reads expected bytes through its
inherited fixture descriptor. Neither observer script nor helper argv/environment
contains them. Profile-wide marker-visible summaries are removed; labelled
target/interface raw observations and each sysctl marker result provide the
evidence. Global interfaces are global observations, not per-target confinement
proof. The macOS 27 research rerun is an agreed owner check after landing;
production inspection remains refused pending that evidence. Corrected Linux
enforcement remains team work; this daemon runner exposes no platform selector
and its darwin check does not compile Linux-only files. Post-landing CI evidence
is still owed, rather than represented as passing Linux enforcement.

Both Linux CI jobs now run focused verbose ProcessInspection tests under
NO_SKIP, recording source revision, uname and bwrap version before the existing
full suites. These scheduled checks provide no evidence until their logs are
received. Cancellation regression coverage includes a live-fixture synthetic
judgment-stage leak and verifies the outer proof translator preserves the breach
code and inspection step. Ordinary cancelled transcripts still report timeout.
