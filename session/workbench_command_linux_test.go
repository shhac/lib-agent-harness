package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkbenchLinuxEditAndRunSession(t *testing.T) {
	requireWorkbenchBwrap(t)
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
	// A real metadata fixture ensures the final overlay is exercised.
	writeFile(t, filepath.Join(o.WorkDir, ".git", "config"), "metadata\n")
	o.complete = (&scriptedModel{steps: [][]scriptedCall{
		{{"write", "write_file", `{"path":"program.sh","content":"printf hello"}`}},
		{{"run", "run_command", `{"command":"sh program.sh"}`}},
		{{"git", "run_command", `{"command":"echo bad >> .git/config"}`}},
		{{"bounded", "run_command", `{"command":"yes x | head -c 200000"}`}},
		{},
	}}).complete
	s := startAPI(t, o)
	defer closeAPI(t, s)
	runAPITurnToEnd(t, s, "edit and run")
	records, text := readTranscript(t, o.RuntimeHome, s.Ref().ID)
	if !strings.Contains(text, "hello") || !strings.Contains(text, "truncated") {
		t.Fatalf("missing command results: %s", text)
	}
	found := false
	for _, r := range records {
		if r.Type == recordToolResult && r.Call == "git" {
			var result struct {
				ExitCode *int `json:"exit_code"`
			}
			if e := json.Unmarshal([]byte(r.Text), &result); e != nil {
				t.Fatal(e)
			}
			if result.ExitCode == nil || *result.ExitCode <= 0 {
				t.Fatalf("git write did not record failure: %s", r.Text)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing git command transcript result")
	}
	data, e := os.ReadFile(filepath.Join(o.WorkDir, ".git", "config"))
	if e != nil || string(data) != "metadata\n" {
		t.Fatalf("git changed: %q %v", data, e)
	}
	closeAPI(t, s)
	resumed, e := Resume(context.Background(), o, s.Ref())
	if e != nil {
		t.Fatal(e)
	}
	defer closeAPI(t, resumed)
}
