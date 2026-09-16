package native

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

const goldenTranscript = "testdata/claude-transcript.golden"
const goldenCodexTranscript = "testdata/codex-transcript.golden"

var updateGolden = flag.Bool("update-golden", false, "rewrite marker transcript fixtures")

func fixedClock(step time.Duration) func() time.Time {
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time { n++; return base.Add(time.Duration(n) * step) }
}
func TestTranscodeGoldenTranscript(t *testing.T) {
	var out bytes.Buffer
	tr, _ := NewStream("claude", &out, StreamOptions{Structured: true, Clock: fixedClock(500 * time.Millisecond)})
	tr.UserPrompt("Review pull request owner/repo#42.")
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"9f1c2d3e-0000-4444-8888-abcdefabcdef"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Read the diff, then check it against the linked issue."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr diff 42"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"diff --git a/main.go b/main.go\n+ added a line"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/tmp/wd/notes.md"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"no such file","is_error":true}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"StructuredOutput","input":{"decision":"COMMENTED","summary":"Left two inline notes about error handling."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"Structured output provided successfully"}]}}`,
		`{"type":"result","subtype":"success","session_id":"9f1c2d3e-0000-4444-8888-abcdefabcdef","structured_output":{"decision":"COMMENTED","summary":"Left two inline notes about error handling."},"usage":{"input_tokens":12000,"output_tokens":800,"cache_read_input_tokens":30000},"total_cost_usd":0.6231}`,
	} {
		if _, err := tr.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	tr.Close()
	got := out.String()

	if *updateGolden {
		if err := os.WriteFile(goldenTranscript, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", goldenTranscript)
		return
	}
	want, err := os.ReadFile(goldenTranscript)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/review -update-golden)", err)
	}
	if got != string(want) {
		t.Errorf("transcript drifted from the fixture the UI parser reads.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCodexTranscodeGoldenTranscript(t *testing.T) {
	var out strings.Builder
	tr, _ := NewStream("codex", &out, StreamOptions{Clock: fixedClock(500 * time.Millisecond)})
	tr.UserPrompt("Review pull request owner/repo#42.")
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"019f6f77-3c3d-7ce3-966d-d4b2083f4459"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"gh pr diff 42"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"gh pr diff 42","aggregated_output":"diff --git a/main.go b/main.go\n+ added a line","exit_code":0}}`,
		`{"type":"item.started","item":{"id":"i2","type":"command_execution","command":"cat /tmp/wd/notes.md"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"command_execution","command":"cat /tmp/wd/notes.md","aggregated_output":"no such file","exit_code":1}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"{\"decision\":\"COMMENTED\",\"summary\":\"Left two inline notes about error handling.\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":42000,"cached_input_tokens":30000,"output_tokens":800,"reasoning_output_tokens":120}}`,
	} {
		if _, err := tr.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	tr.Close()
	got := out.String()

	if *updateGolden {
		if err := os.WriteFile(goldenCodexTranscript, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", goldenCodexTranscript)
		return
	}
	want, err := os.ReadFile(goldenCodexTranscript)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/review -update-golden)", err)
	}
	if got != string(want) {
		t.Errorf("transcript drifted from the fixture the UI parser reads.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
