# Background priority investigation (LAH-20)

Status: investigation incomplete; this change strengthens the observation test,
not the production priority mechanism. It does not establish why the macOS CI
run at commit `3017616` reported nice -10. The failure and passing rerun are
owner-reported evidence; their logs and runner metadata are not available in
this network-isolated workspace. The existing LAH-23 landing hold remains.

## Observations and limits

- Production calls `Setpriority(PRIO_PGRP, leader, 10)` after `exec.Cmd.Start`
  and before `Notify`, without a readback. A successful syscall is the current
  startup criterion. This investigation has not demonstrated a successful
  syscall leaving the leader at another priority.
- Installed Go 1.27.1's Darwin `syscall/exec_libc2.go` sets the child's process
  group before exec and reports failures through the startup error pipe.
  This argues against a simple race with creation of the leader's group,
  but is not evidence about the failing runner's installed Go or later forks.
- The original detached test helper calls `Setpgid`, not `Setpriority`.
  No explicit helper priority change was found. Exec and runner effects
  remain untested.
- The old test sampled numeric PIDs after sleeps, without proving detached
  readiness or checking birth identities. It did not observe priority inside
  `Notify`. These are gaps in the test's evidence, not a demonstrated cause
  of the reported -10 readings.
- Local environment: Darwin 27.0.0, arm64, Go 1.27.1. `ps` is refused with
  `operation not permitted`. A disposable, waiting shell in its own group
  inherited nice 10; `Getpriority(PRIO_PROCESS)` read 10 before and after a
  refused `Setpriority(PRIO_PGRP)` (`EPERM`). The temporary experiment was
  removed. It neither reproduces the CI failure nor proves lowering worked.

Runner base priority, concurrent forks/exec, and mistaken process observations
therefore remain open hypotheses. A test-only change must not be called the
fix merely because the new fixture passes. In particular, the controlled
fixtures deliberately postpone forking until after notification; they cannot
prove that a target which forks or detaches during startup is covered. The
original shell startup/fork path remains a separate subtest, with its early
sleep child and original detached helper; only its final sleep is replaced by
a pipe wait to keep the leader alive during observation.

## Permanent test changes

`TestBackgroundLowersTheWholeTree` retains exact nice-10 assertions. It checks
the live leader inside `Notify`, releases it through a pipe, waits for the
descendant's report after successful detachment, and compares self-reported
priority, platform-correct `getpriority`, and `ps`. Birth identities bracket
the external observations; `ps` also reports PID, PPID and PGID. The original
shell fixture additionally waits for externally verified detachment and
compares `getpriority` and `ps`; its original helper does not self-report nice.
Both controlled shell and direct Go helpers are tested. The test records OS, architecture, Go version
and parent nice, and verifies that parent nice is unchanged.

Cleanup is registered before `Run`, closes the release pipe, closes the process
handle, and awaits `Run` settlement. Readiness waits have a deadline and monitor
early `Run` errors; the external priority observation has its own deadline.
Unix CI adds a verbose, strict `-race -count=20` run so future per-commit logs
will contain repetition evidence without counting capability skips.

No production mechanism, environmental priority probe, public API or
`harness.Support` claim changed. Windows background refusal is unchanged.
No temporary instrumentation or diagnostic branch is included.

## Validation record

- Baseline and revised daemon `run_check`: exit 0 for the project's vet/race
  check. This is sandbox-suite evidence, not every-platform CI evidence or
  proof that capability-gated tests ran without skips.
- Local `go vet ./...`: exit 0.
- Local `go test -race ./...`: exit 0, with environmental capability skips
  permitted. This is not an unsandboxed strict-suite result.
- Local `CGO_ENABLED=0 GOPROXY=off go build ./...`: exit 0.
- Windows/amd64, CGO disabled, network disabled: `go test -c` cross-compiled
  all 26 packages returned by `go list ./...`; packages with no tests emitted
  no executable. Temporary outputs were removed. This is not Windows runtime
  or race-test evidence.
- Local `go test -race ./process -run '^TestBackgroundLowersTheWholeTree$'
  -count=20 -v`: 20 skips due to refused group priority, **zero qualifying
  repetitions**. Parent nice was 10. Do not combine this with future runs.
- Local strict `AGENT_HARNESS_TEST_NO_SKIP=1 go test -race ./process
  -run '^TestBackgroundLowersTheWholeTree$' -count=1 -v`: exit 1 before target
  launch because the group-priority probe returned `EPERM`. Strict mode correctly
  refuses to count this environmental skip as a pass.

## Remaining evidence and implementation

The confirmed GitHub handoff still needs to supply the original failure and
rerun logs, their runner/Go metadata, and per-commit macOS and Linux logs.
Any temporary production-stage diagnostics belong on a separate never-merged
PR, not in this implementation. Compare the original shell fixture with the
controlled fixtures; instrument startup, syscall result, immediate readback,
notification and completed detachment using only bounded fixed fields.

If the product is shown defective, establish priority before the target can
fork when post-start correction cannot suffice, verify the effective result,
and fail closed with containment cleanup. Add the planned setup-failure,
inherited-priority, cancellation and concurrency tests with that mechanism.
No such repair or its failure-path tests are claimed here.

Before landing, obtain an unskipped complete 20-run proof on macOS and Linux
(or a deterministic failing-before/passing-after reproduction), all existing
macOS/Linux/Windows and bubblewrap-0.8.0 CI results, and the product-versus-test
conclusion with commit IDs and CI links. Clear the existing landing hold only
when LAH-23 lands. The task is not complete and this test change is not proof
that production background startup is correct.
