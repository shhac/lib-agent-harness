//go:build !windows

package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Subagents admits Claude Code's Agent tool and nothing else: the OS sandbox,
// the deny rules and the process-wide disallowed tools stay exactly as they
// were, so a subagent cannot reach a tool its parent was refused.
func TestSandboxedClaudeSubagents(t *testing.T) {
	type settingsDoc struct {
		Sandbox     json.RawMessage
		Permissions struct{ Allow, Deny []string }
	}
	read := func(args []string) settingsDoc {
		var doc settingsDoc
		if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	tools := func(args []string) []string {
		value, _ := argValue(args, "--tools")
		return strings.Split(value, ",")
	}
	for _, write := range []bool{true, false} {
		work := t.TempDir()
		off := mustNormalize(t, Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Sandbox: &Sandbox{Write: write}})
		on := mustNormalize(t, Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Sandbox: &Sandbox{Write: write, Subagents: true}})
		offArgs := commandArgs(off, "id", false, &launch{extra: claudeSandboxArgs(off)})
		onArgs := commandArgs(on, "id", false, &launch{extra: claudeSandboxArgs(on)})

		if slices.Contains(tools(offArgs), "Agent") || slices.Contains(tools(offArgs), "Task") {
			t.Fatalf("write=%v: subagents enabled by default: %v", write, offArgs)
		}
		if !slices.Equal(tools(onArgs), append(tools(offArgs), "Agent")) {
			t.Fatalf("write=%v: tools %v, want %v plus Agent", write, tools(onArgs), tools(offArgs))
		}
		if offArgs[slices.Index(offArgs, "--disallowedTools")+1] != "WebFetch,WebSearch,mcp__*" || onArgs[slices.Index(onArgs, "--disallowedTools")+1] != "WebFetch,WebSearch,mcp__*" {
			t.Fatalf("write=%v: disallowed tools changed: %v", write, onArgs)
		}
		offDoc, onDoc := read(offArgs), read(onArgs)
		if string(offDoc.Sandbox) != string(onDoc.Sandbox) {
			t.Fatalf("write=%v: subagents changed the OS sandbox:\n%s\n%s", write, offDoc.Sandbox, onDoc.Sandbox)
		}
		if !slices.Equal(offDoc.Permissions.Deny, onDoc.Permissions.Deny) {
			t.Fatalf("write=%v: subagents changed the deny rules: %v", write, onDoc.Permissions.Deny)
		}
		if slices.Contains(offDoc.Permissions.Allow, "Agent") {
			t.Fatalf("write=%v: Agent allowed by default: %v", write, offDoc.Permissions.Allow)
		}
		if !slices.Equal(onDoc.Permissions.Allow, append(slices.Clone(offDoc.Permissions.Allow), "Agent")) {
			t.Fatalf("write=%v: allow rules %v, want %v plus Agent", write, onDoc.Permissions.Allow, offDoc.Permissions.Allow)
		}
	}

	work := t.TempDir()
	asked := Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Sandbox: &Sandbox{}, Policy: Policy{ClaudeTools: []string{"Bash", "Agent"}}}
	var refused *UnsupportedError
	if _, err := normalize(asked); !errors.As(err, &refused) || refused.Code != RefusedSandboxTool {
		t.Fatalf("Agent without Subagents: %v", err)
	}
	asked.Sandbox = &Sandbox{Subagents: true}
	if _, err := normalize(asked); err != nil {
		t.Fatalf("Agent with Subagents: %v", err)
	}
}

func TestSandboxedCodexSubagents(t *testing.T) {
	for _, write := range []bool{true, false} {
		off := codexSandboxArgs(Sandbox{Write: write})
		on := codexSandboxArgs(Sandbox{Write: write, Subagents: true})
		for _, want := range []string{"agents.enabled=false", "features.multi_agent=false", "features.multi_agent_v2=false"} {
			if !slices.Contains(off, want) {
				t.Fatalf("write=%v: default lost %s: %v", write, want, off)
			}
		}
		for _, want := range []string{"agents.enabled=true", "features.multi_agent=true", "features.multi_agent_v2=false"} {
			if !slices.Contains(on, want) {
				t.Fatalf("write=%v: subagents missing %s: %v", write, want, on)
			}
		}
		if slices.Contains(on, "agents.enabled=false") || slices.Contains(on, "features.multi_agent=false") {
			t.Fatalf("write=%v: subagents left switched off: %v", write, on)
		}
		changed := func(args []string) []string {
			return slices.DeleteFunc(slices.Clone(args), func(a string) bool {
				return a == "-c" || strings.HasPrefix(a, "agents.enabled=") || strings.HasPrefix(a, "features.multi_agent=")
			})
		}
		if !slices.Equal(changed(on), changed(off)) {
			t.Fatalf("write=%v: subagents changed more than the multi-agent switches:\n%v\n%v", write, on, off)
		}
	}
}

// Engines without a proven sandbox refuse a sandboxed session before
// Subagents is reached, with the sandbox's own capability.
func TestSandboxedSubagentsRefusedElsewhere(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Grok, harness.Antigravity, harness.Fx, harness.CommandCode} {
		_, err := normalize(Options{Provider: harness.Provider{Engine: engine}, WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Subagents: true}})
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || unsupported.Code != RefusedNotOffered || unsupported.Capability.Usable() || unsupported.Capability.Reason == "" {
			t.Fatalf("%s: %v", engine, err)
		}
	}
}

// Subagents changes what a session can do, so a resume must carry it, and a
// proof of the sandbox without it is not evidence for one with it.
func TestSandboxedSubagentsChangeDigestAndProofKey(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, engine := range []harness.Engine{harness.Claude, harness.Codex} {
		off := mustNormalize(t, sandboxOptions(t, engine, binary, true))
		raw := off
		raw.Sandbox = &Sandbox{Write: true, Subagents: true}
		raw.Policy = Policy{}
		on := mustNormalize(t, raw)
		if reference(off, "id").ConfigHash == reference(on, "id").ConfigHash {
			t.Fatalf("%s: Subagents does not change the session's digest", engine)
		}
		offKey, err := sandboxKey(off, &launch{extra: sandboxArgs(off)})
		if err != nil {
			t.Fatal(err)
		}
		onKey, err := sandboxKey(on, &launch{extra: sandboxArgs(on)})
		if err != nil {
			t.Fatal(err)
		}
		if offKey == onKey {
			t.Fatalf("%s: a proof without Subagents would be reused with it", engine)
		}
	}
}
