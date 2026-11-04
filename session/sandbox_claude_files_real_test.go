//go:build darwin || linux

package session

import (
	"context"
	stdflag "flag"
	"os/exec"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

func TestClaudeReferenceFilesRealProof(t *testing.T) {
	selected := stdflag.Lookup("test.run").Value.String()
	if selected != "TestClaudeReferenceFilesRealProof" && selected != "^TestClaudeReferenceFilesRealProof$" {
		t.Skip("select exact test name to run installed CLI with dummy local provider")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: binary, Home: t.TempDir()}}, WorkDir: t.TempDir(), Model: "claude-opus-5-5", Policy: Policy{ClaudeTools: []string{"Read", "Write", "Edit", "Glob", "Grep"}}, Sandbox: &Sandbox{Write: true}})
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native CLI: %s", version)
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	if err := probeClaudeSandbox(ctx, o); err != nil {
		t.Fatal(err)
	}
	for _, tools := range [][]string{{"Read", "Write", "Edit", "Glob", "Grep"}, {"Bash"}, {"Bash", "Read", "Write", "Edit", "Glob", "Grep"}, {"Read", "Glob", "Grep"}} {
		o.Policy.ClaudeTools = tools
		t.Logf("effective tools: %v", tools)
		observations, err := probeClaudeReferenceFiles(ctx, o)
		for _, r := range observations {
			if r.Tool == "SHA256" {
				t.Logf("sentinel %s | before=%s | after=%s", r.ID, r.Before, r.After)
			} else {
				t.Logf("%s | %s | denied=%t", r.ID, r.Tool, r.Denied)
			}
			if r.Detail != "" {
				t.Logf("fixture result: %s", r.Detail)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
