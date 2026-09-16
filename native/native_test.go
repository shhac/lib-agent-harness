package native

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestArgsProtectPositionalsAndReassertPolicy(t *testing.T) {
	for _, engine := range []string{"codex", "claude"} {
		c := Config{Engine: engine, Model: "model", Effort: "high", Sandbox: "workspace-write", PermissionMode: "auto", Args: []string{"--arbitrary-variadic", "value"}}
		r := Request{Prompt: "-user prompt", ResumeSession: "session", Schema: `{"type":"object"}`, SchemaPath: "schema.json", OutputPath: "last.json"}
		got, err := Args(c, r)
		if err != nil {
			t.Fatal(err)
		}
		terminator := -1
		for i, a := range got {
			if a == "--" {
				terminator = i
			}
		}
		want := []string{r.Prompt}
		if engine == "codex" {
			want = []string{r.ResumeSession, r.Prompt}
		}
		if terminator < 0 || !reflect.DeepEqual(got[terminator+1:], want) {
			t.Fatalf("%s positional leak: %q", engine, got)
		}
		joined := strings.Join(got, " ")
		if engine == "codex" && !strings.Contains(joined, `sandbox_mode="workspace-write"`) {
			t.Fatal(joined)
		}
		if engine == "claude" && (!strings.Contains(joined, "--resume session") || !strings.Contains(joined, "--permission-mode auto")) {
			t.Fatal(joined)
		}
	}
}

func TestInstructionsAppendWithoutReplacingNativePrompt(t *testing.T) {
	for _, engine := range []string{"codex", "claude"} {
		args, err := Args(Config{Engine: engine}, Request{Prompt: "hello", AppendInstructions: "extra"})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--system-prompt ") {
			t.Fatal("replaced base instructions", joined)
		}
		if !strings.Contains(joined, "extra") {
			t.Fatal(joined)
		}
	}
}

func TestStreamNormalizesUsageAcrossResumes(t *testing.T) {
	cases := []struct {
		engine, first, second string
		want                  TokenUsage
	}{
		{"codex", `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10}}`, `{"type":"turn.completed","usage":{"input_tokens":150,"cached_input_tokens":30,"output_tokens":15}}`, TokenUsage{Input: 120, CacheRead: 30, Output: 15}},
		{"claude", `{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":80,"cache_read_input_tokens":20,"output_tokens":10}}`, `{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":40,"cache_read_input_tokens":30,"output_tokens":5}}`, TokenUsage{Input: 120, CacheRead: 50, Output: 15}},
	}
	for _, tc := range cases {
		t.Run(tc.engine, func(t *testing.T) {
			s, _ := NewStream(tc.engine, io.Discard, StreamOptions{Structured: true})
			for _, line := range []string{tc.first, tc.second} {
				s.UserPrompt("continue")
				io.WriteString(s, line)
				s.Close()
			}
			got := s.Snapshot()
			if got.Usage != tc.want {
				t.Fatalf("got %+v want %+v", got.Usage, tc.want)
			}
			var raw []json.RawMessage
			if json.Unmarshal([]byte(got.RawUsage), &raw) != nil || len(raw) != 2 {
				t.Fatal(got.RawUsage)
			}
		})
	}
}

func TestToolEventsHaveStableIDs(t *testing.T) {
	for _, engine := range []string{"codex", "claude"} {
		t.Run(engine, func(t *testing.T) {
			var events []Event
			s, _ := NewStream(engine, io.Discard, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
			data := `{"type":"item.started","item":{"id":"tool-1","type":"command_execution","command":"echo hi"}}` + "\n" + `{"type":"item.completed","item":{"id":"tool-1","type":"command_execution","command":"echo hi","aggregated_output":"hi","exit_code":0}}`
			if engine == "claude" {
				data = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"echo hi"}}]}}` + "\n" + `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"hi"}]}}`
			}
			io.WriteString(s, data)
			s.Close()
			if len(events) != 2 || events[0].Kind != "tool_start" || events[1].Kind != "tool_end" || events[0].ItemID != "tool-1" || events[1].ItemID != "tool-1" || events[1].Output != "hi" {
				t.Fatalf("events: %+v", events)
			}
		})
	}
}

func TestNewTurnNeverReusesPreviousReport(t *testing.T) {
	s, _ := NewStream("claude", io.Discard, StreamOptions{Structured: true})
	s.UserPrompt("first")
	io.WriteString(s, `{"type":"result","structured_output":{"ok":true}}`)
	s.Close()
	if _, err := s.Report(); err != nil {
		t.Fatal(err)
	}
	s.UserPrompt("next")
	io.WriteString(s, `{"type":"result","is_error":true,"result":"failed"}`)
	s.Close()
	if _, err := s.Report(); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("stale report: %v", err)
	}
}

func TestUnstructuredClaudeResultIsNotFailure(t *testing.T) {
	s, _ := NewStream("claude", io.Discard, StreamOptions{})
	io.WriteString(s, `{"type":"result","subtype":"success","result":"hello"}`)
	s.Close()
	r := s.Snapshot()
	if r.Failure != "" || string(r.Report) != `"hello"` {
		t.Fatalf("%+v", r)
	}
}

// A synthetic executable exercises native Run without invoking any model.
func TestRunSyntheticHarness(t *testing.T) {
	if os.Getenv("NATIVE_HARNESS_HELPER") == "1" {
		for _, arg := range os.Args {
			if arg == "--sleep" {
				time.Sleep(time.Minute)
			}
		}
		if os.Getenv("NATIVE_EXPECT_HOME") != "" && os.Getenv("CODEX_HOME") != os.Getenv("NATIVE_EXPECT_HOME") {
			os.Exit(9)
		}
		fmtLine := `{"type":"thread.started","thread_id":"test-session"}` + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":3}}` + "\n"
		os.Stdout.WriteString(fmtLine)
		for i, arg := range os.Args {
			if arg == "--output-last-message" && i+1 < len(os.Args) {
				os.WriteFile(os.Args[i+1], []byte(`{"ok":true}`), 0600)
			}
		}
		os.Exit(0)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "response.json")
	// The Go test binary's flags precede CLI arguments; the -- separator stops its
	// own flag parser, while the child still sees the native invocation unchanged.
	c := Config{Engine: "codex", Binary: binary, Home: home, Args: []string{}, Env: append(os.Environ(), "NATIVE_HARNESS_HELPER=1", "NATIVE_EXPECT_HOME="+home)}
	// Native Args starts with exec, which stops flag parsing and lets this helper
	// test run with its sentinel. Other tests don't start subprocesses first.
	var transcript bytes.Buffer
	s, _ := NewStream("codex", &transcript, StreamOptions{})
	result, err := Run(context.Background(), c, Request{Prompt: "hi", OutputPath: out, SchemaPath: "schema.json"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "test-session" || string(result.Report) != `{"ok":true}` || result.Usage.Input != 20 {
		t.Fatalf("%+v", result)
	}
	if !strings.Contains(transcript.String(), "session id: test-session") {
		t.Fatal(transcript.String())
	}
}

func TestCancelledContextDoesNotStartHarness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Execute(ctx, Config{Engine: "codex", Binary: "definitely-missing-harness"}, nil, "", io.Discard, io.Discard); err == nil {
		t.Fatal("cancelled invocation succeeded")
	}
}

func TestRunRejectsMissingAndFailedTerminalResults(t *testing.T) {
	cases := []struct{ engine, wire string }{
		{"codex", ""},
		{"codex", `{"type":"item.started","item":{"type":"agent_message","text":"unfinished"}}`},
		{"codex", `{"type":"turn.failed","error":{"message":"provider failed"}}`},
		{"claude", `{"type":"assistant","message":{"content":[{"type":"text","text":"unfinished"}]}}`},
		{"claude", `{"type":"result","is_error":true,"result":"provider failed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.engine+tc.wire, func(t *testing.T) {
			c := Config{Engine: tc.engine, RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
				io.WriteString(out, tc.wire)
				return nil
			}}
			if _, err := Run(context.Background(), c, Request{Prompt: "hello"}, nil); err == nil {
				t.Fatal("accepted missing/failed terminal result")
			}
		})
	}
}

func TestRunPlainTextResultHasSameJSONShape(t *testing.T) {
	for _, engine := range []string{"codex", "claude"} {
		t.Run(engine, func(t *testing.T) {
			wire := `{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}` + "\n" + `{"type":"turn.completed"}`
			if engine == "claude" {
				wire = `{"type":"result","subtype":"success","result":"hello"}`
			}
			c := Config{Engine: engine, RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
				io.WriteString(out, wire)
				return nil
			}}
			r, err := Run(context.Background(), c, Request{Prompt: "hello"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(r.Report) != `"hello"` {
				t.Fatalf("%s report=%s", engine, r.Report)
			}
			if _, err := json.Marshal(r); err != nil {
				t.Fatalf("result not serializable: %v", err)
			}
		})
	}
}

func TestRunNeverUsesStaleOutputFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "last.json")
	if err := os.WriteFile(file, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Engine: "codex", RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
		io.WriteString(out, `{"type":"turn.completed"}`)
		return nil
	}}
	r, err := Run(context.Background(), c, Request{Prompt: "resume", ResumeSession: "session", OutputPath: file, SchemaPath: "schema.json"}, nil)
	if err == nil || len(r.Report) != 0 {
		t.Fatalf("stale report reused: %+v %v", r, err)
	}
}

func TestCodexUpdateDoesNotFinishTool(t *testing.T) {
	var events []Event
	s, _ := NewStream("codex", io.Discard, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
	for _, line := range []string{
		`{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"work"}}`,
		`{"type":"item.updated","item":{"id":"c1","type":"command_execution","aggregated_output":"progress"}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"command_execution","aggregated_output":"done","exit_code":0}}`,
	} {
		io.WriteString(s, line+"\n")
	}
	s.Close()
	if len(events) != 3 || events[0].Kind != "tool_start" || events[1].Kind != "tool_output" || events[2].Kind != "tool_end" || events[2].Failed {
		t.Fatalf("%+v", events)
	}
}

func TestDefaultClaudeHomeSelection(t *testing.T) {
	env := []string{"HOME=/users/me", "CLAUDE_CONFIG_DIR=/other"}
	if !isDefaultClaudeHome("/users/me/.claude", env) || isDefaultClaudeHome("/users/me/other", env) {
		t.Fatal("wrong native home classification")
	}
	if got := withoutEnv(env, "CLAUDE_CONFIG_DIR"); !reflect.DeepEqual(got, []string{"HOME=/users/me"}) {
		t.Fatalf("override was retained: %v", got)
	}
}

func TestUsageAvailabilityDistinguishesZeroFromAbsent(t *testing.T) {
	for _, wire := range []string{`{"type":"result","result":"ok"}`, `{"type":"result","result":"ok","usage":{"input_tokens":0},"total_cost_usd":0}`} {
		s, _ := NewStream("claude", io.Discard, StreamOptions{})
		io.WriteString(s, wire)
		s.Close()
		r := s.Snapshot()
		want := strings.Contains(wire, "usage")
		if r.UsageKnown != want || r.CostKnown != want {
			t.Fatalf("availability %+v for %s", r, wire)
		}
	}
}

func TestClaudeInterruptedPlaceholderAccountingStaysUnknown(t *testing.T) {
	for _, cost := range []string{"0", "0.75"} {
		t.Run(cost, func(t *testing.T) {
			var events []Event
			var transcript bytes.Buffer
			s, _ := NewStream("claude", &transcript, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
			s.UserPrompt("work")
			io.WriteString(s, `{"type":"assistant","message":{"content":[{"type":"text","text":"Already doing work"}]}}`+"\n")
			io.WriteString(s, `{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"total_cost_usd":`+cost+`}`)
			s.Close()
			r := s.Snapshot()
			if r.UsageKnown || r.CostKnown || r.CostUSD != 0 {
				t.Fatalf("interrupted accounting must be unknown, not free/stale: %+v", r)
			}
			last := events[len(events)-1]
			if last.Kind != "usage" || last.UsageKnown || last.CostKnown {
				t.Fatalf("event claimed known accounting: %+v", last)
			}
			if !strings.Contains(r.RawUsage, `"output_tokens":0`) {
				t.Fatal("lost diagnostic raw payload", r.RawUsage)
			}
		})
	}
}

func TestClaudeUnknownInvocationMakesWholeSessionIncomplete(t *testing.T) {
	for _, interrupted := range []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input_tokens":0,"output_tokens":0},"total_cost_usd":0}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Interrupted without result"}]}}`,
	} {
		t.Run(interrupted, func(t *testing.T) {
			s, _ := NewStream("claude", io.Discard, StreamOptions{})
			turn := func(wire string) { s.UserPrompt("continue"); io.WriteString(s, wire); s.Close() }
			turn(`{"type":"result","result":"first","usage":{"input_tokens":100,"output_tokens":10},"total_cost_usd":0.5}`)
			if r := s.Snapshot(); !r.UsageKnown || !r.CostKnown {
				t.Fatalf("first result unknown: %+v", r)
			}
			turn(interrupted)
			r := s.Snapshot()
			if r.UsageKnown || r.CostKnown || r.Usage.Input != 100 || r.CostUSD != 0.5 {
				t.Fatalf("must retain prior evidence without claiming completeness: %+v", r)
			}
			turn(`{"type":"result","result":"last","usage":{"input_tokens":20,"output_tokens":2},"total_cost_usd":0.1}`)
			r = s.Snapshot()
			if r.UsageKnown || r.CostKnown || r.Usage.Input != 120 || r.Usage.Output != 12 || r.CostUSD != 0.6 {
				t.Fatalf("later per-invocation report cannot repair missing turn: %+v", r)
			}
		})
	}
}

func TestClaudeErrorCanCarryUsableAccounting(t *testing.T) {
	s, _ := NewStream("claude", io.Discard, StreamOptions{})
	io.WriteString(s, `{"type":"result","is_error":true,"result":"tool failed","usage":{"input_tokens":100,"output_tokens":7},"total_cost_usd":0.1}`)
	s.Close()
	r := s.Snapshot()
	if !r.UsageKnown || !r.CostKnown || r.Usage.Total() != 107 || r.CostUSD != 0.1 {
		t.Fatalf("discarded actual error-run usage: %+v", r)
	}
}

func TestCodexInterruptedTurnDoesNotReusePreviousKnownTotal(t *testing.T) {
	s, _ := NewStream("codex", io.Discard, StreamOptions{})
	s.UserPrompt("first")
	io.WriteString(s, `{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`)
	s.Close()
	if !s.Snapshot().UsageKnown {
		t.Fatal("first total unavailable")
	}
	s.UserPrompt("next")
	io.WriteString(s, `{"type":"turn.failed","error":{"message":"interrupted"}}`)
	s.Close()
	if r := s.Snapshot(); r.UsageKnown || r.Usage.Input != 100 {
		t.Fatalf("reused prior total as complete: %+v", r)
	}
	s.UserPrompt("recover")
	io.WriteString(s, `{"type":"turn.completed","usage":{"input_tokens":180,"output_tokens":18}}`)
	s.Close()
	if r := s.Snapshot(); !r.UsageKnown || r.Usage.Input != 180 {
		t.Fatalf("cumulative total should repair prior gap: %+v", r)
	}
}
