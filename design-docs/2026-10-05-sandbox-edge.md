# Harness-owned session edge

LAH-31 implements the agreed macOS Part 1; Linux bubblewrap edge is Part 2.
LAH-19 established external egress under Codex network-on. The edge reuses
LAH-25 execution rules and LAH-39/40 all-interface loopback and port refusals.
Existing native modes and API workbench file-tool refusals remain intact.

## Boundary

Sandbox.Edge selects the shared deny-default command Seatbelt profile with
private runtime home/tmp, explicit reads and paired read/execution selectors.
The resolved CLI installation is admitted only after checking that it contains
neither the selected CLI home nor a known native credential at its root. Known credential checks are bounded to installation roots; caller Read grants retain the shared semantics and are not recursively walked. The original login home
is denied. Writes respect Write and protect workspace .git, .codex, .agents, .claude and .mcp.json.
Descendants inherit the boundary. Codex danger-full-access and Claude
sandbox.enabled=false avoid nested Seatbelt. Approval never/dontAsk and tool
selection remain. The edge replaces native filesystem/network containment,
not application authorization or semantic tool safety.

Claude requires a caller setup-token and private RuntimeHome/claude config,
without owner history, settings or keychain access. Codex retains private
import-only shared-login reconciliation. Runtime auth.json, login records and configuration are protected against descendant writes; edge runtime credentials never flow back into the owner login. A prior poisoned runtime is overwritten from the source on preparation. Codex refresh is blocked at auth.openai.com, including caller host/base-URL overrides: allowing an in-memory refresh without persisting its rotated token would consume the owner's refresh token and invalidate the native login. An authenticated CONNECT refusal to that host latches edge_login_refresh_blocked and stops the session. Refresh the selected owner login outside the edge, then resume; no edge credential is exported. Writable transcript/cache state may persist across resumes, and remains untrusted; Claude config agents/, commands/ and skills/ and Codex runtime prompts/ and rules/ are write-protected. Protecting known configuration files is not a general guarantee that a CLI will never interpret some new writable state file as instructions. Shells can inspect credentials and proxy auth.
Scrubbing protects library output, not encoded secrets or malicious private
files. Provider-host exfiltration remains possible. No keychain or
mDNSResponder mach-lookup is added. Loopback permits all local interfaces;
local-only and selected ports refuse. Hosted control directories must be outside runtime home and workspace. Only the channel secret is readable, with a literal socket transport grant. Lease, launch and bridge-lock state are denied to descendants. The confined bridge does not acquire a writable bridge lock; the library owns the assignment lease and launch record, and recovery still requires live process identity. Bridges must use this version of RunBridge. Direct shell access to the authenticated channel is admitted, with the same active-turn, tool allow-list and serialized caller admission; this is not isolation of native tools from hosted handlers.

## Transport

Each session owns a random-credential CONNECT listener on 127.0.0.1. Both cases
of HTTP_PROXY/HTTPS_PROXY are configured; NO_PROXY contains only loopback names
when requested. Exact allowed destinations are Codex api.openai.com,
chatgpt.com:443 or Claude api.anthropic.com:443, plus
Edge.AllowHosts and the HTTPS Edge.BaseURL authority. Wildcards/IP literals
refuse. The proxy pins all validated DNS addresses and tries them in order,
rejects loopback/link-local/unspecified destinations, and permits private
addresses only for explicitly caller-listed hosts. Shared CGNAT/Tailscale and benchmark ranges are refused for default hosts too. Headers are bounded and
idle timeout measures activity in both directions. Credentials cannot be used
across sessions. Admitted localhost services must be trusted: they can relay
traffic. Another process with the same user identity can steal private state;
this is process confinement, not same-user isolation.

## Proof and lifecycle

The pre-launch canary uses disposable homes, dummy credentials, a fake TLS
provider and a proxy unable to dial real providers. The proxy maps the engine default hosts (and configured BaseURL authority) to that fake provider. Production provider selection and BaseURL are preserved, without a custom Codex model_provider. Codex proves the boundary once, with a dummy API-key login; Claude uses the OAuth environment path. A dummy ChatGPT-login pass ran the same plan and script and was dropped when Codex 0.166 began requiring ChatGPT's private account-routing replies before a turn; a real ChatGPT login reaching an unlisted host fails closed at the proxy. Unsupported transports or startup services refuse instead of accepting evidence from a substitute provider. Scripted native shell-tool
results must prove positive reads/execution, requested writes, denied outside
reads/writes/.git writes, grandchild confinement, requested loopback bind/reach
to an outside host witness, forbidden CONNECT, direct TCP443/TCP53/UDP53 denial
and DNS failure. Unsandboxed controls must succeed; missing controls, silence
or partial output refuse. Observed escapes outrank incomplete evidence.
Hosted tools require channel discovery without invoking caller handlers.

Only complete successes enter a serialized process-local cache keyed by binary
identity, edge template/profile, reference policy, environment and tool contract.
Launch rechecks streamed binary identity, including the npm vendored native executable and adjacent Codex code-mode host. Edge references add revision, hosts and BaseURL
without tokens, endpoints or credentials. Legacy hashes remain unchanged.
Policy mismatches refuse before login work. Cancellation reaps the canary tree,
closes proxy/provider and removes disposable state without caching. Real runtime
preparation follows proof and is idempotent; failures return no reference.
Start and Resume share proof and preparation. VerifySandbox shares only proof: it neither imports credentials nor creates a real tool host or holds its lease. CLI exit and Close reap the
process tree then close the proxy, even with uncertain settlement. Proxy failure
fails the session with edge_proxy_failed, never direct-egress fallback.
Launcher death removes the in-process proxy; resume creates fresh transport.

Preflight codes are edge_unavailable, edge_proxy_unavailable, edge_conflict,
edge_token_required. Missing proof reports edge_provider_path,
edge_egress_not_refused, edge_dns_not_refused or edge_confinement_failed.
Interrupted probes report probe_timed_out.

## Evidence limits

Unit and fake-CLI tests cover proxy policy, one-way streams, address fallback,
reference binding, engine arguments, scrubbing and lifecycle. The CLI-independent darwin boundary test runs the exact Perl canary under the edge and judges all observations, with loopback both enabled and disabled. Component tests also exercise a fake provider and file witnesses; they do
not establish installed CLI compatibility. A macOS CI step installs pinned
Codex 0.166.0 and Claude Code 2.1.316 from npm and selects the complete canary with dummy
credentials, recording versions and binary hashes. Absent selection or skipped
tests provide no enforcement evidence. The strict daemon run_check experiment also refused nested sandbox-exec with exit status 71 (NO_SKIP=1 made this a failure, not evidence). The ordinary suite can pass with environment refusals skipped; that is not OS evidence. Pinned Claude 2.1.282 is absent locally, and network access is prohibited, so the pinned installed-CLI job has not been executed here. Support remains Unknown;
production refuses unless its complete installed-binary canary succeeds.
Linux edge claims no host-development-service access.
LAH-21 read-contract integration, caller path requests, workbench restoration,
browser access, Linux edge and consumer adoption remain separate tasks.

Production and canary both set DISABLE_AUTOUPDATER=1, DISABLE_TELEMETRY=1 and MAX_RETRIES=0; the obsolete HARNESS_PROBE_KEY selector is removed. This environment contract is bound by edge revision provider-v5 in proofs and references. Only disposable credentials, fake-provider CA trust and boundary observation variables differ during proof.

Unavailable unsandboxed network, DNS or host-listener controls report edge_control_unavailable, never observed_escape; restore control connectivity and retry. Actual observed escapes retain their egress/DNS/confinement facts and take precedence over incomplete observations.

Resume-policy mismatch and preflight-conflict tables currently run on darwin only. Linux and Windows exercise the platform refusal, not edge resume binding. Linux Part 2 must extend those tests to bubblewrap before offering support.

The fake-provider SSL_CERT_FILE/NODE_EXTRA_CA_CERTS overrides substitute trust for the canary. This does not prove native production TLS trust-root loading or certificate evaluation under Seatbelt. Security.framework/trustd access and Rust native-root loader behavior remain unproved; production provider TLS may fail after a passing proxy canary. No mach-lookup grant is inferred or added. Support's Unknown reason names this limit explicitly.

The proxy binds tcp4 127.0.0.1 only. Seatbelt remote ip localhost:PORT admits [::1]:PORT too. A same-user process binding that IPv6 port can relay traffic from the confined tree even without Loopback; proxy authentication does not protect a different listener. This is an explicit same-user threat-model limit, not proof of IPv4-only proxy transport isolation.

## Owner evidence: draft 5, 620c609

The owner ran outside any sandbox on macOS 27.0; OS build and architecture were not supplied.

- `AGENT_HARNESS_TEST_NO_SKIP=1 go test -race -count=1 -v ./session -run TestEdge`: exit 1. TestEdgeRealBoundaryScript refused its unsandboxed control; TestEdgeFixtureLifecycleAndSecrets failed its private Claude state comparison. The owner reported all other TestEdge tests passed. This is partial evidence, not a complete passing boundary canary.
- Scratch-prefix npm Codex 0.160.0 and Claude Code 2.1.282 installed-CLI canaries failed. Codex could not print its version inside the edge (exit 1; stderr was discarded). Claude printed version 2.1.282, binary SHA-256 `fcfd837103965c64de34a6b9b94370d77a347ea71819715a27d5f0ef01775ea4`, edge executable identity `853c2b4a20c8d514a989ac358826ad8922910c58234a3d24bedb69fe31b99bb8`, then refused the control.
- No credentialed real-provider turn was run. The owner accepted native TLS remaining Unknown and reserved that live check for after landing.

The confirmed control bug required MagicDNS 100.100.100.100 to answer TCP 443. The corrected proof retains the positively proved DNS TCP/UDP 53 resolver and separately resolves and reaches an off-machine provider TCP 443 witness. It sends no API request or credential and performs no inference. Every required port still needs a positive outside control; an unavailable witness refuses with edge_control_unavailable. Boundary output records both addresses.

The Claude comparison used unresolved RuntimeHome against the normalized launch path (/var versus /private/var). The fixture now compares resolved paths and diagnoses config and token separately. Independent environment and plan tests pin private HOME, CLAUDE_CONFIG_DIR and the token without nested Seatbelt.

Codex's exact startup cause remains unconfirmed because stderr was discarded. Shared installation discovery now handles optional-package aliases independently of manifest names, resolves Node from the launch PATH, includes interpreter and native vendor files in executable identity, and invokes Node explicitly with paired literal read/exec grants plus ancestor metadata access. It does not admit Node's whole directory or widen the shared filtered PATH. Missing components and credential-overlapping installations refuse with actionable edge_conflict reasons. Version failures now record bounded, secret-scrubbed stderr.

These corrections have fixture coverage, not corrected installed-CLI enforcement evidence. Rerun the strict TestEdge suite and pinned installed-CLI step on the corrected source, recording source, versions, hashes, controls and complete results. No passing rerun has been received; generic passing checks and environment skips cannot establish enforcement.

Edge profile v4 protects the known runtime instruction loader directories; provider revision v5 invalidates older edge proof/reference reuse. The LAH-29 ProcessInspection feature and its capability error remain intact after merging main. Native edge sessions do not gain that inspection capability. Partial failed owner evidence is recorded below; corrected installed-CLI enforcement remains unproved. Landed LAH-47 standalone and hosted timeout ceilings remain unchanged.

## Port to v0.31.0 and first passing outside-sandbox run (2026-11-01)

LAH-31 draft 6 (bc165e5) was re-applied onto v0.31.0 as one three-way patch.
The edge keeps the v13 shared command profile, including its scratch
Unix-socket grants, and adds only the proxy port, the protections above and,
for Codex, the com.openai.codex preference domain through cfprefsd: Codex
0.166 refuses thread/start when it cannot synchronize its managed (MDM)
preferences. No other preference domain, keychain or trustd lookup is added.
Claude 2.1.316 makes its temporary directory under /tmp regardless of TMPDIR,
so the edge sets CLAUDE_CODE_TMPDIR to the runtime scratch. Provided skills
are refused under the edge, and the runtime home's skills/ loader is
write-protected. Edge revision seatbelt-edge-v5-provider-v6 invalidates draft
proofs and references.

The canary's fake provider now presents a leaf signed by a disposable CA
(webpki refuses a certificate that is both), the canary also sets Codex's
CODEX_CA_CERTIFICATE, and its scripted shell call names the disposable
workspace. Native TLS trust remains unproved.

Run outside any sandbox on macOS 27.0 (26A428), arm64, with
`AGENT_HARNESS_TEST_NO_SKIP=1`: every `./session -run TestEdge` test and every
`./sandbox -run Edge` test passed, including TestEdgeRealBoundaryScript with
loopback on and off. The installed-CLI canary
(`AGENT_HARNESS_EDGE_CLI_CANARY=1 ... -run '^TestEdgeInstalledCLICanary$'`)
passed for Codex 0.166.0 (standalone, binary SHA-256
39acf979729dd290f9a24ee38b2914e036c91b65952872e28ae2a6b88a7db7c1, edge
identity a0275712883a361d036e8283163aa364b47e9070678dfec678398af65a83f07c)
and Claude Code 2.1.316 (native, binary SHA-256
64c327e92b2c5e0f78c0a980c5a518121e1f0d140ca771ca5dd4488d1a2dcb3f, edge
identity 6eeee56dc353a3cc84019ea71b8ec28ffb23c5a87c62c211d574cbf21b39c18e).
No credentialed provider turn was run; Support stays Unknown.

A standalone Codex installed under the owner's ~/.codex (as here) overlaps the
selected login home and is refused with edge_conflict; production needs a
credential-free installation outside it.

## Per-engine entries, Antigravity and fx (2026-11-01)

The edge's engine switches moved into an `edge` entry in the session engine
registry: provider hosts, the refresh host it refuses, preference domains,
engine-owned variables, protected loaders, single-file reads, pseudo-terminal
admission, arguments, environment, BaseURL variable, post-proof preparation,
hosted tools, and the canary's fake-provider wire and drive. Support is still
the static table, and a test holds it equal to the registry. Codex and Claude
entries carry their previous behaviour unchanged: their profiles, plan keys,
arguments, environments, references and edge revision are as before, and the
pinned edge template digest still holds.

Hosts were established by running each CLI through a CONNECT proxy that logged
host:port only, with a disposable home and dummy or no credentials.

Antigravity (agy 1.4.12): logged out, it reaches antigravity-unleash.goog,
antigravity-cli-auto-updater-974169037036.us-central1.run.app,
storage.googleapis.com and play.googleapis.com. Its Google-account login is
held in the macOS Keychain ("Antigravity CLI"); a dummy
`~/.gemini/oauth_creds.json` is ignored. That login cannot be imported one way
into a file, and the edge admits no keychain, so the edge runs agy's
documented Gemini API-key mode with a caller key (`Edge.GeminiAPIKey`):
inference is `POST generativelanguage.googleapis.com/v1beta/models/<model>:streamGenerateContent?alt=sse`,
the public Gemini API, which the canary fakes. agy completes a turn with
every other host refused. Go's macOS verifier needs trustd (OSStatus -26276
without it); with `SSL_CERT_FILE` it uses that bundle instead, so production
names `/private/etc/ssl/cert.pem` (a literal read grant) and the canary its
disposable CA. agy listens on random localhost ports for its language server,
and a canary without Loopback does not complete, so Loopback is required. It
runs commands in a pseudo-terminal: the profile admits `pseudo-tty`,
`/dev/ptmx` and only `/dev/ttys*` carrying the `com.apple.sandbox.pty`
extension, as Codex's own Seatbelt policy does. The canary opens a terminal
outside the sandbox and requires the shell to fail to open it; with the
extension requirement removed, the canary reports observed_escape. agy writes
`settings.json` back on load by atomic rename and falls back to defaults if
refused, so the library writes it in agy's own indented form; it creates an
empty `mcp_config.json` if missing, and resolves a default project under
`config/projects` with its id in `antigravity-cli/cache`, so the library
writes all four before every launch. The runtime home is the session's own,
so these writes, made outside Seatbelt, follow no link: each folder from the
runtime home down is opened without following links and created where
missing, a link or non-folder component and a non-plain target are refused,
and each file is written fresh beside its target and renamed over it, so a
planted hard-link alias keeps the old inode.

All of `RuntimeHome/.gemini` is read-only to descendants. Rather than naming
loaders, which missed `config/rules`, `config/agents`,
`antigravity-cli/{rules,skills,plugins}` and `.gemini/skills` in the first
draft, the edge denies the whole tree and then admits only the state agy
writes, as observed under 1.4.12: `antigravity-cli/{annotations,brain,
conversations,crashes,implicit,log,presence}`, `.gemini/antigravity`, its
summary database and bookkeeping files (each with the siblings extending its
name, such as `-wal` and atomic temporaries), three cache files, the default
project file, `config/.migrated`, and `bin/agentapi`, the shell integration
agy rewrites before every command it runs. The rest of `bin/`, where agy
installs executables, stays read-only, as do its builtin tree (agy logs that
it cannot populate it and carries on) and its updater state. Hard links are
denied throughout. The canary creates folders for, and requires a refused
write to, a sample of loaders no rule names: plugin hooks, CLI and config
rules, agents, skills, a plugin manifest, `bin/webm_encoder`, `GEMINI.md`,
settings and the MCP list. agy still completes its turn. Workspace
`AGENTS.md` and `GEMINI.md` are the repository's own files, as Codex's and
Claude's are; the workspace's `.agents`, where agy keeps project rules,
skills, plugins and MCP servers, is protected as for every engine.

The canary allows its one command with a `permissions.allow` rule, written
only in the disposable runtime: API-key logins run only in request-review,
where unruled commands are denied.

agy is declared standalone: its installation is one native executable,
granted as that file alone. A symlinked agy (`~/.local/bin/agy ->
.../agy-arm64`) is its target file; a launcher script is refused, never a
folder grant. For every engine, an executable the edge launches or grants
(CLI, helpers, interpreter, tool bridge) inside the workspace or runtime
home, or reached through an alias of either, is refused: the session could
replace it, and the next proof would run and admit the replacement.

Loopback is a requirement for Antigravity, so its edge session's egress is
not provider-only: descendants reach every on-machine TCP service, including
a local proxy that could relay further, and binds cover all local
interfaces, as for any edge session with Loopback.

`AGY_CLI_DISABLE_AUTO_UPDATE=1` did not stop agy's background updater: an
unconfined observation run replaced the installed agy 1.4.2 with 1.4.12.
Under the edge the updater's hosts are refused and the installation is not
writable.

fx (0.0.14): with every proxy variable naming the logging proxy and a dummy
`AI_GATEWAY_API_KEY`, fx reported the gateway's HTTP 401 while the proxy saw
no connection; with only loopback egress allowed it fails ("fx: Unexpected").
fx does not use the proxy, so under the edge it cannot reach a provider, and
SandboxEdge is Unsupported for it. An installed-binary test keeps that tied to
the binary. From strings alone (unverified by observation): its providers use
ai-gateway.vercel.sh and api.vercel.com, chatgpt.com/backend-api/codex with
refresh at auth.openai.com, cli-chat-proxy.grok.com, api.x.ai and auth.x.ai;
upgrades come from releases.fx.sh; `FX_DISABLE_KEYCHAIN` exists. Its state is
file-backed under `~/.fx`.

Evidence, outside any sandbox on macOS 27.0 (26A428), arm64, Go 1.27.3: the
strict `./session -run TestEdge` and `./sandbox -run Edge` suites pass, and
the installed-CLI canary passes for Codex 0.166.0, Claude Code 2.1.316 and
agy 1.4.12 (binary SHA-256
34423fc9b0af3841e7646aa72a2d40ffeab8218b22d1cb99eac7355f9eba31e3, edge
identity a8dfd506eaea4fd17d352e250c482d82fd1b867083cb269dddc7eb5e723bf74f).
Not verified: a real Gemini API key under the edge, production TLS through the
system bundle, and agy's Google-account login (the owner's agy is not logged
in). Support stays Unknown.

The installed-agy canary runs only locally. CI installs pinned Codex and
Claude Code from npm and runs their canary; it does not install agy, whose
vendor installer pipes a downloaded script into a shell, and adding that to
CI would run it there. An agy release that changes its terminals, provider
wire, settings or loaders is therefore caught only by running the canary by
hand, which should precede relying on a new agy:

    PATH=$HOME/.local/bin:$PATH AGENT_HARNESS_TEST_NO_SKIP=1 \
      AGENT_HARNESS_TEST_LOOPBACK_CONTROL=1.1.1.1 \
      AGENT_HARNESS_EDGE_CLI_CANARY=1 \
      AGENT_HARNESS_EDGE_CLI_CANARY_ENGINES=antigravity,fx \
      go test -race -count=1 -v ./session \
      -run '^(TestEdgeInstalledCLICanary|TestEdgeInstalledFxIgnoresProxy)$'

That command runs agy only inside the edge. Run unconfined, agy's updater
replaces its own installed binary even with AGY_CLI_DISABLE_AUTO_UPDATE=1.
