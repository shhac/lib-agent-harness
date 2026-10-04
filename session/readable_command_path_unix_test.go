//go:build darwin || linux

package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostedAndCompatibilityReadableCommandPath(t *testing.T) {
	requireReadableCommandPlatform(t)
	hidden, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	readable, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{filepath.Join(hidden, "node"): "#!/bin/sh\necho unsafe\n", filepath.Join(readable, "node"): "#!/bin/sh\necho readable\n"} {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Commands: &Commands{Read: []string{readable}, Env: []string{"PATH=" + hidden + ":" + readable + ":/usr/bin:/bin"}}}
	script := filepath.Join(o.WorkDir, "env-node")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env node\n"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"command": script})
	o.complete = (&scriptedModel{steps: [][]scriptedCall{{{"path", "run_command", string(raw)}}, {}}}).complete
	s := startAPI(t, o)
	defer closeAPI(t, s)
	runAPITurnToEnd(t, s, "run")
	records, _ := readTranscript(t, o.RuntimeHome, s.Ref().ID)
	found := false
	for _, record := range records {
		if record.Type == recordToolResult && record.Call == "path" {
			var result CommandResult
			if err := json.Unmarshal([]byte(record.Text), &result); err != nil {
				t.Fatal(err)
			}
			if result.Stdout != "readable\n" || strings.Count(result.Stderr, "[harness PATH:") != 1 {
				t.Fatal(result)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing hosted result")
	}
	wrapper, err := OpenCommandSandbox(context.Background(), CommandSandboxOptions{WorkDir: o.WorkDir, RuntimeHome: privateHome(t), Read: []string{readable}, Env: o.Workbench.Commands.Env})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := wrapper.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := wrapper.Run(context.Background(), CommandRequest{Command: script})
	if err != nil || result.Stdout != "readable\n" || strings.Count(result.Stderr, "[harness PATH:") != 1 {
		t.Fatal(result, err)
	}
}
