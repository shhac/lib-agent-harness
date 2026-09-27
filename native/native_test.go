package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/tomltest"
)

func TestArgsProtectPositionalsAndReassertPolicy(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		c := Config{Provider: harness.Provider{Engine: engine}, Model: "model", Effort: "high", Args: []string{"--arbitrary-variadic", "value"}}
		if engine == harness.Codex {
			c.Codex.Sandbox = "workspace-write"
		} else {
			c.Claude.PermissionMode = "auto"
		}
		r := Request{Prompt: "-user prompt", ResumeSession: "session", Schema: `{"type":"object"}`}
		if err := validate(c, r); err != nil {
			t.Fatal(err)
		}
		got := buildArgs(c, r, &codexReport{schema: "schema.json", output: "last.json"})
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

// Codex reads these overrides as TOML. They were JSON-encoded before, which
// TOML mostly shares; each value must still read back as what that encoding
// meant, including the U+FFFD it substituted for invalid UTF-8, so no caller's
// instructions change meaning or start being refused.
func TestCodexOverridesKeepTheirMeaningAsTOML(t *testing.T) {
	instructions := "Line one\nsay \"hi\" \\ <b>&</b> \u2028 bell\a del\x7f \U0001F600 bad\xff\xfe bytes"
	c := Config{Provider: harness.Provider{Engine: harness.Codex}, Effort: "high", Codex: CodexOptions{Sandbox: "workspace-write"}}
	args := buildArgs(c, Request{Prompt: "p", ResumeSession: "session", AppendInstructions: instructions}, nil)
	meant := func(value string) string {
		raw, _ := json.Marshal(value)
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	want := map[string]string{"developer_instructions": meant(instructions), "model_reasoning_effort": "high", "sandbox_mode": "workspace-write"}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		key, value, err := tomltest.Override(args[i+1])
		if err != nil {
			t.Fatalf("%q is not a TOML override: %v", args[i+1], err)
		}
		if value != want[key] {
			t.Errorf("%s reads back as %q, want %q", key, value, want[key])
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("overrides missing: %v", want)
	}
}

func TestInstructionsAppendWithoutReplacingNativePrompt(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		args := buildArgs(Config{Provider: harness.Provider{Engine: engine}}, Request{Prompt: "hello", AppendInstructions: "extra"}, nil)
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
		engine        harness.Engine
		first, second string
		want          harness.Usage
	}{
		// Codex reports the session total, with cached input inside input_tokens.
		{harness.Codex, `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10}}`, `{"type":"turn.completed","usage":{"input_tokens":150,"cached_input_tokens":30,"output_tokens":15}}`, harness.Usage{Known: true, Input: 150, CacheRead: 30, Output: 15, CacheKnown: true}},
		// Claude reports each invocation, with cached input outside input_tokens.
		{harness.Claude, `{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":80,"cache_read_input_tokens":20,"output_tokens":10}}`, `{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":40,"cache_read_input_tokens":30,"output_tokens":5}}`, harness.Usage{Known: true, Input: 170, CacheRead: 50, Output: 15, CacheKnown: true}},
	}
	for _, tc := range cases {
		t.Run(string(tc.engine), func(t *testing.T) {
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
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
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
	s, _ := NewStream(harness.Claude, io.Discard, StreamOptions{Structured: true})
	s.UserPrompt("first")
	io.WriteString(s, `{"type":"result","structured_output":{"ok":true}}`)
	s.Close()
	if _, err := s.Report(); err != nil {
		t.Fatal(err)
	}
	s.UserPrompt("next")
	io.WriteString(s, `{"type":"result","is_error":true,"result":"provider says no"}`)
	s.Close()
	_, err := s.Report()
	if facts, ok := harness.ErrorFacts(err); !ok || facts.Code != CodeTurnFailed || strings.Contains(err.Error(), "provider says no") {
		t.Fatalf("stale report or provider text in error: %v", err)
	}
	if s.Snapshot().Failure != "provider says no" {
		t.Fatalf("failure text lost: %+v", s.Snapshot())
	}
}

func TestUnstructuredClaudeResultIsNotFailure(t *testing.T) {
	s, _ := NewStream(harness.Claude, io.Discard, StreamOptions{})
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
		if code := os.Getenv("NATIVE_EXIT"); code != "" {
			os.Stdout.WriteString(`{"type":"turn.failed","error":{"message":"provider text"}}` + "\n")
			os.Exit(3)
		}
		if os.Getenv("NATIVE_EXPECT_HOME") != "" && os.Getenv("CODEX_HOME") != os.Getenv("NATIVE_EXPECT_HOME") {
			os.Exit(9)
		}
		fmtLine := `{"type":"thread.started","thread_id":"test-session"}` + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":3}}` + "\n"
		os.Stdout.WriteString(fmtLine)
		schema := ""
		for i, arg := range os.Args {
			if arg == "--output-schema" && i+1 < len(os.Args) {
				data, _ := os.ReadFile(os.Args[i+1])
				schema = string(data)
			}
		}
		if schema != `{"type":"object"}` {
			os.Exit(8)
		}
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
	// The Go test binary's flags precede CLI arguments; the -- separator stops its
	// own flag parser, while the child still sees the native invocation unchanged.
	c := Config{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Binary: binary, Home: home}}, Env: append(os.Environ(), "NATIVE_HARNESS_HELPER=1", "NATIVE_EXPECT_HOME="+home)}
	// Native Args starts with exec, which stops flag parsing and lets this helper
	// test run with its sentinel. Other tests don't start subprocesses first.
	var transcript bytes.Buffer
	s, _ := NewStream(harness.Codex, &transcript, StreamOptions{Structured: true})
	result, err := Run(context.Background(), c, Request{Prompt: "hi", Schema: `{"type":"object"}`}, s)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "test-session" || string(result.Report) != `{"ok":true}` || result.Usage.Input != 20 || !result.Usage.Known || result.Usage.CacheKnown {
		t.Fatalf("%+v", result)
	}
	if !strings.Contains(transcript.String(), "session id: test-session") {
		t.Fatal(transcript.String())
	}

	c.Env = append(c.Env, "NATIVE_EXIT=3")
	result, err = Run(context.Background(), c, Request{Prompt: "hi"}, nil)
	facts, ok := harness.ErrorFacts(err)
	if !ok || facts.Family != harness.FailureProcess || facts.Code != CodeProcessExited || facts.ExitCode == nil || *facts.ExitCode != 3 || facts.Operation != harness.Run || facts.Engine != harness.Codex {
		t.Fatalf("exit facts: %+v %v", facts, err)
	}
	if strings.Contains(err.Error(), "provider text") || !strings.Contains(result.Failure, "provider text") {
		t.Fatalf("provider text belongs in Result.Failure only: %v %+v", err, result)
	}

	c.Provider.CLI.Binary = filepath.Join(t.TempDir(), "definitely-missing-harness")
	_, err = Run(context.Background(), c, Request{Prompt: "hi"}, nil)
	if facts, _ := harness.ErrorFacts(err); facts.Family != harness.FailurePreflight || facts.Code != CodeExecutableNotFound {
		t.Fatalf("missing binary: %+v %v", facts, err)
	}
}

func TestCancelledContextReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	c := Config{Provider: harness.Provider{Engine: harness.Codex}, RunCommand: func(context.Context, []string, string, io.Writer, io.Writer) error {
		called = true
		return nil
	}}
	if _, err := Run(ctx, c, Request{Prompt: "hi"}, nil); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled invocation started or lost its context error: called=%v err=%v", called, err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	c.RunCommand = func(context.Context, []string, string, io.Writer, io.Writer) error {
		cancel()
		return errors.New("signal: killed")
	}
	if _, err := Run(ctx, c, Request{Prompt: "hi"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during the run: %v", err)
	}
}

func TestRunRejectsMissingAndFailedTerminalResults(t *testing.T) {
	cases := []struct {
		engine harness.Engine
		wire   string
		family harness.Family
		code   string
	}{
		{harness.Codex, "", harness.FailureProcess, CodeNoTerminalResult},
		{harness.Codex, `{"type":"item.started","item":{"type":"agent_message","text":"unfinished"}}`, harness.FailureProcess, CodeNoTerminalResult},
		{harness.Codex, `{"type":"turn.failed","error":{"message":"provider failed"}}`, harness.FailureTurn, CodeTurnFailed},
		{harness.Codex, `{"type":"turn.completed"}`, harness.FailureTurn, CodeNoResponse},
		{harness.Claude, `{"type":"assistant","message":{"content":[{"type":"text","text":"unfinished"}]}}`, harness.FailureProcess, CodeNoTerminalResult},
		{harness.Claude, `{"type":"result","is_error":true,"result":"provider failed"}`, harness.FailureTurn, CodeTurnFailed},
	}
	for _, tc := range cases {
		t.Run(string(tc.engine)+tc.wire, func(t *testing.T) {
			c := Config{Provider: harness.Provider{Engine: tc.engine}, RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
				io.WriteString(out, tc.wire)
				return nil
			}}
			_, err := Run(context.Background(), c, Request{Prompt: "hello"}, nil)
			facts, ok := harness.ErrorFacts(err)
			if !ok || facts.Family != tc.family || facts.Code != tc.code || facts.Engine != tc.engine || facts.Operation != harness.Run || facts.Retryable {
				t.Fatalf("facts %+v for %v", facts, err)
			}
			if strings.Contains(err.Error(), "provider failed") {
				t.Fatalf("provider text in error: %v", err)
			}
		})
	}
}

func TestRunPlainTextResultHasSameJSONShape(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
			wire := `{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}` + "\n" + `{"type":"turn.completed"}`
			if engine == "claude" {
				wire = `{"type":"result","subtype":"success","result":"hello"}`
			}
			c := Config{Provider: harness.Provider{Engine: engine}, RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
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

func flagValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// Codex reads its schema and writes its report through files. Each invocation
// gets its own private directory outside the workspace, so the agent cannot
// forge the report and a resume can never read the previous one.
func TestCodexReportDirectoryIsPrivateAndFreshPerRun(t *testing.T) {
	workDir := t.TempDir()
	var dirs []string
	var wroteReport bool
	c := Config{Provider: harness.Provider{Engine: harness.Codex}, RunCommand: func(_ context.Context, args []string, _ string, out, _ io.Writer) error {
		schemaPath, outputPath := flagValue(args, "--output-schema"), flagValue(args, "--output-last-message")
		dir := filepath.Dir(schemaPath)
		if filepath.Dir(outputPath) != dir {
			t.Errorf("schema and report in different directories: %q %q", schemaPath, outputPath)
		}
		dirs = append(dirs, dir)
		if within(resolved(workDir), resolved(dir)) {
			t.Errorf("report directory %s is inside the workspace %s", dir, workDir)
		}
		if schema, err := os.ReadFile(schemaPath); err != nil || string(schema) != `{"type":"object"}` {
			t.Errorf("schema file: %q %v", schema, err)
		}
		if info, err := os.Stat(dir); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
			t.Errorf("report directory is not private: %v %v", info, err)
		}
		if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
			t.Errorf("a report existed before the run: %v", err)
		}
		io.WriteString(out, `{"type":"thread.started","thread_id":"s"}`+"\n"+`{"type":"item.completed","item":{"type":"agent_message","text":"{\"streamed\":true}"}}`+"\n"+`{"type":"turn.completed"}`+"\n")
		if wroteReport {
			return os.WriteFile(outputPath, []byte(`{"ok":true}`), 0o600)
		}
		return nil
	}}
	s, _ := NewStream(harness.Codex, nil, StreamOptions{Structured: true})
	wroteReport = true
	r, err := Run(context.Background(), c, Request{Prompt: "first", WorkDir: workDir, Schema: `{"type":"object"}`}, s)
	if err != nil || string(r.Report) != `{"ok":true}` || string(s.Snapshot().Report) != `{"ok":true}` {
		t.Fatalf("first run: %+v %v", r, err)
	}
	wroteReport = false
	r, err = Run(context.Background(), c, Request{Prompt: "resume", ResumeSession: "s", WorkDir: workDir, Schema: `{"type":"object"}`}, s)
	if facts, _ := harness.ErrorFacts(err); facts.Code != CodeReportUnavailable || len(r.Report) != 0 {
		t.Fatalf("a resume without a report must not reuse a stale or streamed one: %+v %v", r, err)
	}
	if len(dirs) != 2 || dirs[0] == dirs[1] {
		t.Fatalf("each run needs its own directory: %q", dirs)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("report directory %s was left behind: %v", dir, err)
		}
	}
}

func TestCodexReportDirectoryIsNeverInsideTheWorkspace(t *testing.T) {
	workDir := t.TempDir()
	previous := reportRoots
	t.Cleanup(func() { reportRoots = previous })
	reportRoots = func() []string { return []string{filepath.Join(workDir, "reports")} }
	called := false
	c := Config{Provider: harness.Provider{Engine: harness.Codex}, RunCommand: func(context.Context, []string, string, io.Writer, io.Writer) error {
		called = true
		return nil
	}}
	_, err := Run(context.Background(), c, Request{Prompt: "p", WorkDir: workDir, Schema: `{"type":"object"}`}, nil)
	if facts, _ := harness.ErrorFacts(err); facts.Family != harness.FailurePreflight || facts.Code != CodeReportDirUnavailable || called {
		t.Fatalf("report directory inside the workspace was accepted: called=%v %v", called, err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "reports")); !os.IsNotExist(err) {
		t.Fatal("a refused report directory was created in the workspace")
	}

	reportRoots = func() []string {
		return []string{filepath.Join(workDir, "reports"), filepath.Join(t.TempDir(), "elsewhere")}
	}
	c.RunCommand = func(_ context.Context, args []string, _ string, out, _ io.Writer) error {
		if within(resolved(workDir), resolved(flagValue(args, "--output-schema"))) {
			t.Error("fell back into the workspace")
		}
		io.WriteString(out, `{"type":"turn.completed"}`)
		return os.WriteFile(flagValue(args, "--output-last-message"), []byte(`{"ok":true}`), 0o600)
	}
	if _, err := Run(context.Background(), c, Request{Prompt: "p", WorkDir: workDir, Schema: `{"type":"object"}`}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCodexPlainRunWritesNoFiles(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Codex}, RunCommand: func(_ context.Context, args []string, _ string, out, _ io.Writer) error {
		if slices.Contains(args, "--output-schema") || slices.Contains(args, "--output-last-message") {
			t.Errorf("unstructured run passed report files: %q", args)
		}
		io.WriteString(out, `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`+"\n"+`{"type":"turn.completed"}`)
		return nil
	}}
	if r, err := Run(context.Background(), c, Request{Prompt: "p"}, nil); err != nil || string(r.Report) != `"hi"` {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCodexMalformedReportIsTyped(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Codex}, RunCommand: func(_ context.Context, args []string, _ string, out, _ io.Writer) error {
		io.WriteString(out, `{"type":"turn.completed"}`)
		return os.WriteFile(flagValue(args, "--output-last-message"), []byte(`not json`), 0o600)
	}}
	_, err := Run(context.Background(), c, Request{Prompt: "p", Schema: `{"type":"object"}`}, nil)
	if facts, _ := harness.ErrorFacts(err); facts.Family != harness.FailureTurn || facts.Code != CodeMalformedReport {
		t.Fatalf("%v", err)
	}
}

func TestCodexUpdateDoesNotFinishTool(t *testing.T) {
	var events []Event
	s, _ := NewStream(harness.Codex, io.Discard, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
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
		s, _ := NewStream(harness.Claude, io.Discard, StreamOptions{})
		io.WriteString(s, wire)
		s.Close()
		r := s.Snapshot()
		want := strings.Contains(wire, "usage")
		if r.Usage.Known != want || r.Cost.Known != want {
			t.Fatalf("availability %+v for %s", r, wire)
		}
	}
}

func TestClaudeInterruptedPlaceholderAccountingStaysUnknown(t *testing.T) {
	for _, cost := range []string{"0", "0.75"} {
		t.Run(cost, func(t *testing.T) {
			var events []Event
			var transcript bytes.Buffer
			s, _ := NewStream(harness.Claude, &transcript, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
			s.UserPrompt("work")
			io.WriteString(s, `{"type":"assistant","message":{"content":[{"type":"text","text":"Already doing work"}]}}`+"\n")
			io.WriteString(s, `{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"total_cost_usd":`+cost+`}`)
			s.Close()
			r := s.Snapshot()
			if r.Usage.Known || r.Cost.Known || r.Cost.USD != 0 {
				t.Fatalf("interrupted accounting must be unknown, not free/stale: %+v", r)
			}
			last := events[len(events)-1]
			if last.Kind != "usage" || last.Usage.Known || last.Cost.Known {
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
			s, _ := NewStream(harness.Claude, io.Discard, StreamOptions{})
			turn := func(wire string) { s.UserPrompt("continue"); io.WriteString(s, wire); s.Close() }
			turn(`{"type":"result","result":"first","usage":{"input_tokens":100,"output_tokens":10},"total_cost_usd":0.5}`)
			if r := s.Snapshot(); !r.Usage.Known || !r.Cost.Known {
				t.Fatalf("first result unknown: %+v", r)
			}
			turn(interrupted)
			r := s.Snapshot()
			if r.Usage.Known || r.Cost.Known || r.Usage.Input != 100 || r.Cost.USD != 0.5 {
				t.Fatalf("must retain prior evidence without claiming completeness: %+v", r)
			}
			turn(`{"type":"result","result":"last","usage":{"input_tokens":20,"output_tokens":2},"total_cost_usd":0.1}`)
			r = s.Snapshot()
			if r.Usage.Known || r.Cost.Known || r.Usage.Input != 120 || r.Usage.Output != 12 || r.Cost.USD != 0.6 {
				t.Fatalf("later per-invocation report cannot repair missing turn: %+v", r)
			}
		})
	}
}

func TestClaudeErrorCanCarryUsableAccounting(t *testing.T) {
	s, _ := NewStream(harness.Claude, io.Discard, StreamOptions{})
	io.WriteString(s, `{"type":"result","is_error":true,"result":"tool failed","usage":{"input_tokens":100,"output_tokens":7},"total_cost_usd":0.1}`)
	s.Close()
	r := s.Snapshot()
	if !r.Usage.Known || !r.Cost.Known || r.Usage.Total() != 107 || r.Cost.USD != 0.1 {
		t.Fatalf("discarded actual error-run usage: %+v", r)
	}
}

func TestCodexInterruptedTurnDoesNotReusePreviousKnownTotal(t *testing.T) {
	s, _ := NewStream(harness.Codex, io.Discard, StreamOptions{})
	s.UserPrompt("first")
	io.WriteString(s, `{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`)
	s.Close()
	if !s.Snapshot().Usage.Known {
		t.Fatal("first total unavailable")
	}
	s.UserPrompt("next")
	io.WriteString(s, `{"type":"turn.failed","error":{"message":"interrupted"}}`)
	s.Close()
	if r := s.Snapshot(); r.Usage.Known || r.Usage.Input != 100 {
		t.Fatalf("reused prior total as complete: %+v", r)
	}
	s.UserPrompt("recover")
	io.WriteString(s, `{"type":"turn.completed","usage":{"input_tokens":180,"output_tokens":18}}`)
	s.Close()
	if r := s.Snapshot(); !r.Usage.Known || r.Usage.Input != 180 {
		t.Fatalf("cumulative total should repair prior gap: %+v", r)
	}
}
