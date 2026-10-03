# Sandbox stage B: review inventory

v0.23.0 moves the command sandbox into sandbox and retains the v0.22 session
opener/types as Deprecated wrappers for one release. No public name is removed,
no Support claim changes and neither consumer needs a change.

## A. Mechanical moves only

Each source below is `session/<filename>`, now `sandbox/<filename>`.
Only package/import/type/option-field identifiers change; fixture literals,
canary text and assertions are retained.

- `workbench_bwrap_unix.go`: argument phases, tool checks and fingerprints;
  CapabilityError becomes ProofError.
- `workbench_canary_unix.go`: Linux canaries and judges, unchanged bodies.
- `workbench_layout_unix.go`: layout/environment/shell quoting;
  workbenchCapability now constructs the equivalent sandbox ProofError.
- `workbench_system_other.go`: platform system set and containment.
- `workbench_probekey_darwin_test.go`: flattened option access; literal
  seatbelt-workbench-v4 and the profile digest remain unchanged.
- `workbench_probekey_linux_test.go`: flattened option access; literal
  bwrap-workbench-v2, independent JSON payload and session scratch shapes
  remain unchanged.
- `workbench_probetemplate_unix_test.go`: package only; frozen bwrap builder,
  system set, flags, modes and argument fixture remain unchanged.
- `workbench_adversary_linux_test.go`: package only; raw syscall/mount adversaries.
- `workbench_env_test.go`: package only; Env denylist assertions.

Seatbelt profile/args, bwrap arguments/checks/fingerprint, Linux canary bodies
and judges, and macOS canary/judge/probe execution bodies were compared directly
with the original source before hand-over. No body difference was found after
equivalent typed-error identifier substitution. No key or template pin literal
was updated. Every original test name in the affected suites remains present.

## B. Files with integration or logic changes

All paths in this section are destinations; the original basename is retained
where code moved from session.

- `sandbox/workbench_seatbelt_darwin.go`: normalization consumes flat Options
  and the shared normalization-mode signature; the profile, arguments and
  proof-key payload are unchanged; informational identity shares the key's
  executable read, with no extra failure point.
- `sandbox/workbench_bwrap_linux.go`: standalone/hosted normalization mode
  replaces Workbench's private flag, at the same refusal point; test trial hook.
- `sandbox/workbench_probe_darwin.go`: returns opaque Proof, including informational macOS
  binary identity derived from the existing key read, instead of mutating
  Workbench; uses sandbox's cache.
- `sandbox/workbench_probe_linux.go`: returns system set/binary/identity in
  Proof; still re-proves on every call and records only successful evidence.
- `sandbox/cache.go`: new command-only copy of the existing synchronized cache,
  with the same greater-than-64 reset threshold.
- `sandbox/options.go`: extracted timeout/read/alias/Env normalization, private
  RuntimeHome and WorkDir separation checks; standalone and hosted entry points
  preserve their refusal ordering; internal read-directory rules also serve native CLI
  read normalization; normalization entry points remain unexported.
- `sandbox/runner.go`: Proof/Runner API and matching-evidence constructor;
  original request and normalized options are frozen by Proof; runner preparation
  checks matching evidence without re-normalizing after proof. Callers supply a
  valid result budget and canonical private state within RuntimeHome.
- `sandbox/workbench_command_unix.go`: preparation takes Proof and explicit
  stateDir; token, scratch, environment/output/status logic and formats retain
  their prior behavior; exposes the canary counter through the internal hook.
- `sandbox/workbench_command_darwin.go`: runner receives flat options, Proof,
  explicit state and standalone budget selection; returns CommandError.
- `sandbox/workbench_command_linux.go`: same runner inputs and CommandError;
  both binary-identity checks and command settlement logic are retained.
- `sandbox/workbench_command_other.go`: unsupported Prove refuses explicitly;
  command preparation remains unavailable.
- `sandbox/command_state_unix.go`: standalone state belongs to Sandbox,
  uses sandbox StateError/ErrStateLocked and preserves locks/sweep order.
- `sandbox/command_state_other.go`: sandbox-owned unsupported state stub.
- `sandbox/state_{unix,windows}.go`, `sandbox/state.go`: unexported lock/private-dir
  mechanisms with identical session.lock path and native lock semantics.
- `sandbox/sandbox.go`: standalone API renamed to Open/Options/Sandbox;
  Workspace and Runner are owned directly; limit, close, Start and Run
  semantics remain intact.
- `sandbox/id.go`: production copy of the existing UUID helper, now also used
  by the relocated tests; byte format and failure behavior unchanged.
- `sandbox/errors.go`: final command documentation plus StateError,
  StateUnusable/StateLocked and ErrStateLocked, preserving session facts/text.
- `sandbox/hooks.go`, `internal/sandboxhook/hook.go`: preserve workspace hooks;
  add test-only runner injection, cache observations and synthetic sandbox
  construction without public test controls.
- `sandbox/probe_helpers.go`, `internal/sandboxprobe/probe.go`: shared unchanged
  loopback canary, outside witness, listener and port mechanisms, separate from
  CLI probe orchestration.
- `sandbox/command_sandbox{,_unix,_darwin,_linux}_test.go`: relocated standalone
  suites now use Open; their fakes own Workspace/Runner directly. The hosted
  result contract stays in session.
- `sandbox/command_state_unix_test.go`: flat state setup and independently
  serialized v0.22 token/lock fixtures, directly and through Open.
- `sandbox/workbench_command_{darwin,linux}_test.go`: split by test function;
  OS/runner tests move, model-session/transcript tests remain in session.
- `sandbox/workbench_bwrap_{unix,linux}_test.go`: typed proof facts replace
  session error fields; split pure checks/key tests from hosted launch tests.
- `sandbox/runner_helpers_test.go`: test-only adapter preserves the original
  mixed runners' setup and JSON assertions without importing session.
- `sandbox/cache_test.go`, `sandbox/runner_test.go`: cache reset/race and
  failed-evidence/result-budget/private-state tests before state preparation.
- `sandbox/helpers_test.go`: removes the duplicate UUID helper, now production.
- `session/command_sandbox.go`: tooling-recognized Deprecated paragraphs on
  wrappers, aliases, constants and methods; restored RuntimeHome contract; fromSandbox on all
  errors, with stable translated Close/Result error identity.
- `session/workbench.go`: private command fields collapse to sandbox.Proof;
  public Workbench, Commands, tool definitions and digest are unchanged.
- `session/workbench_command.go`: flat option adapter, shared normalization/
  proof/runner calls and the original hosted JSON/error contract.
- `session/workbench_host.go`: commands holds *sandbox.Runner.
- `session/api.go`: command-close errors go through the host's fromSandbox.
- `session/api_state_{unix,windows}.go`: internal lock/private-state adapters over sandbox,
  with StateError translation and unchanged session directory sync behavior.
- `session/sandbox_errors.go`, `session/sandbox_errors_test.go`: add both state
  translations and extend the complete error/facts/message compatibility table.
- `session/sandbox.go`: native read normalization uses shared internal read rules;
  native CLI policy and probes stay in session.
- `session/sandbox_loopback.go`: only pure witness helpers delegate to
  internal/sandboxprobe; native CLI loopback proof stays in session.
- `session/command_sandbox_test.go`: hosted result/cancellation assertions
  remain here and inject a Runner through the internal test hook.
- `session/workbench_command_{darwin,linux}_test.go`,
  `session/workbench_bwrap_linux_test.go`: retained hosted portions use the
  host's adapter; synthetic supported-version checks move to sandbox.
- `session/command_helpers_{unix,darwin,linux}_test.go`: retained token fixture
  shape and platform prerequisites for session-owned tests.
- `session/workbench_recovery_test.go`: injects Runner cleanup through the
  internal hook; original resource-close assertions are retained.
- `session/command_{cache,wrapper,refusal_linux,legacy_unix}_test.go`:
  split-cache isolation/failed proofs; Deprecated-wrapper refusals, command
  errors and settled handles; hosted Linux refusal precedence; independent
  v0.22 session token resume and corrupt-marker preservation.
- `sandbox/doc.go`, `README.md`: final package structure, low-level ownership
  and sandbox.Open examples, with the one-release compatibility note.
- `design-docs/2026-10-03-sandbox-package.md`,
  `design-docs/2026-10-03-command-sandbox.md`,
  `design-docs/2026-09-29-api-workbench.md`: final boundary/cache/lifecycle
  design and pointers; historical proof-version descriptions are clarified.
- `release-notes/v0.22.0.md`, `release-notes/v0.23.0.md`: correct published
  v0.22 history and list every introduced/moved public name for v0.23.
- `sandbox/bridge.go`, `internal/sandboxbridge/{bridge,read}.go`: internal
  integration adapters and shared read rules replace five unintended public
  helper exports; hosted proof does not repeat normalization.
- `session/command_deprecation_test.go`: parses Go documentation to prove
  every retained name has its own deprecation paragraph and the RuntimeHome
  contract remains documented.
- `sandbox/boundary_test.go`: prevents integration helpers becoming public API.
- `sandbox/runner_test.go`: proves original and normalized options match frozen
  evidence without repeated discovery, and mutated request slices are refused.
