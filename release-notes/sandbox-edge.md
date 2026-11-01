# Sandbox edge (additive, opt-in)

Edge revision seatbelt-edge-v5-provider-v6, ported onto v0.31.0, is the
first to pass its installed-CLI canary outside any sandbox (Codex 0.166.0,
Claude Code 2.1.316). Codex may read its own com.openai.codex preference
domain; Claude's temporary directory is the runtime scratch; provided skills
are refused under the edge; the canary proves the boundary once with a dummy
API-key login. Earlier draft proofs and references refuse reuse.

Profile v4/provider revision v5 separates DNS and HTTPS controls, canonicalizes
private Claude state, and admits npm alias vendors and Node interpreters
explicitly. Older edge proofs/references refuse reuse; ordinary references are
unchanged. Corrected installed-CLI enforcement awaits the owner's offered rerun.

Adds session.Sandbox.Edge, session.Edge (AllowHosts, ClaudeToken, BaseURL) and
harness.SandboxEdge. Codex/Claude macOS sessions use shared Seatbelt, private
runtime homes and a credentialed provider-only CONNECT proxy, with native
sandboxes disabled only inside the proved edge. Support is Unknown until the
installed-binary canary succeeds. Linux refuses edge_unavailable pending Part 2;
Windows restricted hosting remains unavailable. Legacy modes/hashes are unchanged.

New codes: edge_unavailable, edge_proxy_unavailable, edge_conflict,
edge_token_required. New facts: edge_provider_path, edge_egress_not_refused,
edge_dns_not_refused, edge_confinement_failed, edge_proxy_failed,
edge_login_refresh_blocked, edge_control_unavailable.
Edge policy participates in resume compatibility; secrets do not.
Claude requires caller setup-token and private config, without owner keychain,
history or settings. macOS loopback permits all-interface binds and inbound;
selected ports, local-only, Browser, Restriction and Claude Web refuse.
See [the contract and evidence limits](../design-docs/2026-10-05-sandbox-edge.md).

Edge revision v2 protects native runtime credentials/configuration and workspace .claude/.mcp.json, disables credential write-back, keeps hosted control records outside writable state, and makes VerifySandbox proof-only. Canary provider selection now matches production, including both default Codex login paths. Older edge draft references deliberately refuse reuse; ordinary references are unchanged.

Provider revision v3 removes Codex auth.openai.com egress and refuses attempts to re-admit it. Expired-token refresh fails with edge_login_refresh_blocked without consuming the owner's rotating token; refresh native login outside the edge and resume. Production and canary share update/telemetry/retry switches. Unavailable proof witnesses report edge_control_unavailable. Previous edge draft references refuse reuse; non-edge references remain unchanged.

Edge profile v3 protects Claude agents/commands/skills and Codex prompts/rules in the runtime home. Support explicitly reports native TLS trust evaluation as unproved; fake-CA canary success does not prove production TLS. The tcp4 listener/IPv6 Seatbelt-port same-user relay limit is documented. Provider revision v4 invalidates prior edge draft references.
