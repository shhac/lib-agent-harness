package native

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

func TestGrokArgsBindEveryValue(t *testing.T) {
	c := Config{
		Provider: harness.Provider{Engine: harness.Grok},
		Model:    "grok-build",
		Effort:   "high",
		Grok:     GrokOptions{Sandbox: "strict", PermissionMode: "dontAsk", Tools: []string{"read", "grep"}},
		Args:     []string{"--no-subagents"},
	}
	r := Request{Prompt: "-review this", ResumeSession: "session", WorkDir: "/work", AppendInstructions: "-be brief", Schema: "{ \"type\": \"object\" }"}
	if err := validate(c, r); err != nil {
		t.Fatal(err)
	}
	got := buildArgs(c, r, nil)
	// grok 1.0.41 refuses `-p -review` ("a value is required for --single")
	// and accepts `--single=-review`.
	want := []string{
		"--single=-review this", "--output-format=streaming-json", "--resume=session", "--cwd=/work",
		"--rules=-be brief", `--json-schema={"type":"object"}`, "--model=grok-build", "--reasoning-effort=high",
		"--sandbox=strict", "--permission-mode=dontAsk", "--tools=read,grep", "--no-subagents",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q want %q", got, want)
	}
}

func TestGrokReducedTelemetryEnvironmentIsOptIn(t *testing.T) {
	base := []string{
		"GROK_TELEMETRY_ENABLED=1",
		"GROK_TELEMETRY_MIXPANEL_ENABLED=1",
		"GROK_TELEMETRY_TRACE_UPLOAD=1",
		"GROK_CURSOR_MCPS_ENABLED=1",
		"UNRELATED=kept",
	}
	grok := harness.Provider{Engine: harness.Grok}
	if got := nativeEnvironment(Config{Provider: grok, Env: base}); !reflect.DeepEqual(got, base) {
		t.Fatalf("default Grok environment changed: %q", got)
	}
	grok.CLI.Home = "/runtime/grok"
	reduced := nativeEnvironment(Config{Provider: grok, Env: base, Grok: GrokOptions{Telemetry: GrokTelemetryReduced}})
	for _, want := range append([]string{"GROK_HOME=/runtime/grok", "UNRELATED=kept"}, grokReducedTelemetryEnvironment...) {
		key, _, _ := strings.Cut(want, "=")
		found := 0
		for _, entry := range reduced {
			if strings.HasPrefix(entry, key+"=") {
				found++
				if entry != want {
					t.Fatalf("%s overridden as %s", want, entry)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%s appears %d times in %q", want, found, reduced)
		}
	}
	if got := nativeEnvironment(Config{Provider: harness.Provider{Engine: harness.Claude}, Env: base, Grok: GrokOptions{Telemetry: GrokTelemetryReduced}}); !reflect.DeepEqual(got, base) {
		t.Fatalf("Grok policy changed Claude environment: %q", got)
	}
}

func runGrokLines(t *testing.T, s *Stream, prompt string, lines ...string) {
	t.Helper()
	s.UserPrompt(prompt)
	for _, line := range lines {
		if _, err := io.WriteString(s, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
}

// Shapes below are trimmed from grok 1.0.41 `--output-format streaming-json`.
func TestGrokStreamReadsTheLiveWireFormat(t *testing.T) {
	var transcript bytes.Buffer
	var events []Event
	s, err := NewStream(harness.Grok, &transcript, StreamOptions{Clock: fixedClock(500 * time.Millisecond), OnEvent: func(e Event) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	runGrokLines(t, s, "inspect the project",
		`{"type":"available_commands","tools":["read_file"],"commands":["compact"]}`,
		`{"type":"thought","data":"Reading first."}`,
		`{"type":"text","data":"Let me look."}`,
		`{"type":"tool_call","toolCallId":"call_1","title":"Read","kind":"read","status":"in_progress","toolName":"read_file","rawInput":{"path":"README.md"},"content":[],"locations":[]}`,
		`{"type":"tool_call_update","toolCallId":"call_1","status":"completed","content":[],"rawOutput":"contents","locations":[]}`,
		`{"type":"usage","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"reasoning_tokens":1},"signature":"x"}`,
		`{"type":"text","data":"I found "}`,
		`{"type":"text","data":"one issue."}`,
		`{"type":"usage","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":0,"reasoning_tokens":0},"signature":"y"}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"grok-session","requestId":"r","usage":{"input_tokens":12,"cache_read_input_tokens":2,"cache_creation_input_tokens":0,"output_tokens":3,"reasoning_tokens":1,"total_tokens":17},"num_turns":2,"total_cost_usd":0.0279,"total_cost_usd_ticks":278940000}`,
	)
	r := s.Snapshot()
	if r.SessionID != "grok-session" || string(r.Report) != `"I found one issue."` {
		t.Fatalf("report must be the final response only: %+v", r)
	}
	// Grok's input_tokens is uncached; the shared Input adds the cache back.
	if r.Usage != (harness.Usage{Known: true, Input: 14, Output: 3, CacheRead: 2, Reasoning: 1, CacheKnown: true, ReasoningKnown: true}) {
		t.Fatalf("per-response usage lines must not be summed with end: %+v", r)
	}
	if fresh, ok := r.Usage.Fresh(); !ok || fresh != 12 {
		t.Fatalf("fresh input %d %v", fresh, ok)
	}
	if r.Cost != (harness.Cost{USD: 0.027894, Known: true}) {
		t.Fatalf("cost is taken from exact ticks: %+v", r)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"reasoning", "message", "tool_start", "tool_end", "message", "message", "session", "usage"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events %q want %q", kinds, want)
	}
	for _, fragment := range []string{"thinking\nReading first.", "session id: grok-session", "succeeded in 500ms", "tokens used\n17", "~ $0.0279 at API rates"} {
		if !strings.Contains(transcript.String(), fragment) {
			t.Fatalf("transcript lacks %q:\n%s", fragment, transcript.String())
		}
	}
}

func TestGrokStreamUnwrapsACPSessionUpdates(t *testing.T) {
	s, _ := NewStream(harness.Grok, nil, StreamOptions{})
	runGrokLines(t, s, "hi",
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`,
		`{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	)
	if r := s.Snapshot(); r.SessionID != "acp-session" || string(r.Report) != `"hello"` {
		t.Fatalf("result: %+v", r)
	}
}

func TestGrokStructuredReportComesFromEnd(t *testing.T) {
	s, _ := NewStream(harness.Grok, nil, StreamOptions{Structured: true})
	runGrokLines(t, s, "greet",
		`{"type":"text","data":"{\"greeting\":\"Hi\"}"}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":1,"output_tokens":1},"structuredOutput":{"greeting":"Hi"}}`,
	)
	if r := s.Snapshot(); string(r.Report) != `{"greeting":"Hi"}` || r.Failure != "" {
		t.Fatalf("result: %+v", r)
	}

	s, _ = NewStream(harness.Grok, nil, StreamOptions{Structured: true})
	runGrokLines(t, s, "greet",
		`{"type":"text","data":"{\"greeting\":\"Hi\"}"}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":1,"output_tokens":1}}`,
	)
	if r := s.Snapshot(); len(r.Report) != 0 || r.Failure == "" {
		t.Fatalf("streamed text must not stand in for structured output: %+v", r)
	}
}

func TestGrokUnfinishedStopReasonsFail(t *testing.T) {
	for _, reason := range []string{"max_tokens", "max_turn_requests", "refusal", "cancelled", "something_new"} {
		s, _ := NewStream(harness.Grok, nil, StreamOptions{})
		runGrokLines(t, s, "hi",
			`{"type":"text","data":"partial"}`,
			`{"type":"end","stopReason":"`+reason+`","sessionId":"s","usage":{"input_tokens":1,"output_tokens":1}}`,
		)
		if r := s.Snapshot(); !strings.Contains(r.Failure, reason) {
			t.Fatalf("%s reported as success: %+v", reason, r)
		}
	}
}

func TestGrokResumedUsageIsSummedAndGapsStayUnknown(t *testing.T) {
	end := func(input, output int, extra string) string {
		return `{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":` + strconv.Itoa(input) + `,"output_tokens":` + strconv.Itoa(output) + `}` + extra + `}`
	}
	s, _ := NewStream(harness.Grok, nil, StreamOptions{})
	runGrokLines(t, s, "first", `{"type":"text","data":"a"}`, end(10, 2, `,"total_cost_usd_ticks":100`))
	runGrokLines(t, s, "second", `{"type":"text","data":"b"}`, end(5, 1, `,"total_cost_usd_ticks":50`))
	r := s.Snapshot()
	if r.Usage != (harness.Usage{Known: true, Input: 15, Output: 3, CacheKnown: true}) || r.Cost != (harness.Cost{USD: 150 / grokCostTicksPerUSD, Known: true}) {
		t.Fatalf("resumed invocations must sum: %+v", r)
	}

	// Cost Grok could not fully account is omitted; absence is not free.
	runGrokLines(t, s, "third", `{"type":"text","data":"c"}`, end(1, 1, `,"cost_is_partial":true`))
	if r := s.Snapshot(); !r.Usage.Known || r.Cost.Known {
		t.Fatalf("partial cost must leave cost unknown: %+v", r)
	}

	// An invocation that dies before `end` keeps the session total partial.
	runGrokLines(t, s, "fourth", `{"type":"text","data":"d"}`)
	runGrokLines(t, s, "fifth", `{"type":"text","data":"e"}`, end(1, 1, ""))
	if r := s.Snapshot(); r.Usage.Known || r.Usage.Input != 17 {
		t.Fatalf("an unaccounted invocation must stay visible: %+v", r)
	}
}

func TestGrokErrorSpendCountsOnceAndOnlyAsPartial(t *testing.T) {
	s, _ := NewStream(harness.Grok, nil, StreamOptions{})
	runGrokLines(t, s, "hi", `{"type":"error","message":"provider failed","usage":{"input_tokens":7,"output_tokens":1}}`)
	r := s.Snapshot()
	if r.Failure != "provider failed" || r.Usage.Input != 7 || r.Usage.Known {
		t.Fatalf("error spend must be recorded as partial: %+v", r)
	}

	s, _ = NewStream(harness.Grok, nil, StreamOptions{})
	runGrokLines(t, s, "hi",
		`{"type":"error","message":"tool failed","usage":{"input_tokens":7,"output_tokens":1}}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":7,"output_tokens":1}}`,
	)
	if r := s.Snapshot(); r.Usage.Input != 7 {
		t.Fatalf("spend reported by both error and end was counted twice: %+v", r)
	}
}

func TestGrokJSONRPCResponseIsNotATerminal(t *testing.T) {
	s, _ := NewStream(harness.Grok, nil, StreamOptions{})
	runGrokLines(t, s, "hi", `{"type":"text","data":"unfinished"}`, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	if s.t.reachedTerminal() {
		t.Fatal("a JSON-RPC response was read as the end of the turn")
	}
}

func TestGrokRunRequiresEndAndRejectsInvalidPolicyBeforeExecution(t *testing.T) {
	for _, wire := range []string{"", `{"type":"text","data":"unfinished"}`, `{"type":"error","message":"provider failed"}`} {
		c := Config{Provider: harness.Provider{Engine: harness.Grok}, RunCommand: func(_ context.Context, _ []string, _ string, out, _ io.Writer) error {
			_, _ = io.WriteString(out, wire)
			return nil
		}}
		if _, err := Run(context.Background(), c, Request{Prompt: "hello"}, nil); err == nil {
			t.Fatalf("accepted Grok stream without end: %s", wire)
		}
	}
	called := false
	c := Config{Provider: harness.Provider{Engine: harness.Grok}, Grok: GrokOptions{Telemetry: 99}, RunCommand: func(context.Context, []string, string, io.Writer, io.Writer) error {
		called = true
		return nil
	}}
	_, err := Run(context.Background(), c, Request{Prompt: "hello"}, nil)
	if facts, _ := harness.ErrorFacts(err); facts.Code != CodeUnsupportedOption || called {
		t.Fatalf("invalid policy reached execution: called=%v err=%v", called, err)
	}
}

func TestGrokRunReturnsStructuredReport(t *testing.T) {
	var args []string
	c := Config{Provider: harness.Provider{Engine: harness.Grok}, RunCommand: func(_ context.Context, a []string, _ string, out, _ io.Writer) error {
		args = a
		_, _ = io.WriteString(out, `{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":1,"output_tokens":1},"structuredOutput":{"ok":true}}`+"\n")
		return nil
	}}
	result, err := Run(context.Background(), c, Request{Prompt: "check", Schema: `{"type":"object"}`}, nil)
	if err != nil || string(result.Report) != `{"ok":true}` {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(args[:3], []string{"--single=check", "--output-format=streaming-json", `--json-schema={"type":"object"}`}) {
		t.Fatalf("args=%q", args)
	}
}

func TestGrokReportIsTheFinalResponseOnly(t *testing.T) {
	s, _ := NewStream(harness.Grok, nil, StreamOptions{})
	runGrokLines(t, s, "hi",
		`{"type":"text","data":"Let me look."}`,
		`{"type":"tool_call","toolCallId":"c","toolName":"read_file"}`,
		`{"type":"tool_call_update","toolCallId":"c","status":"completed","rawOutput":"x"}`,
		`{"type":"usage","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"type":"thought","data":"Nothing more to say."}`,
		`{"type":"usage","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":2,"output_tokens":2}}`,
	)
	if r := s.Snapshot(); len(r.Report) != 0 {
		t.Fatalf("narration before a tool call became the report: %s", r.Report)
	}
}

func TestGrokFailedTurnKeepsNoReport(t *testing.T) {
	for name, lines := range map[string][]string{
		"stop reason": {`{"type":"text","data":"partial"}`, `{"type":"end","stopReason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`},
		"error event": {`{"type":"text","data":"partial"}`, `{"type":"error","message":"boom"}`, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
		"end status":  {`{"type":"text","data":"partial"}`, `{"type":"end","status":"failed","error":{"message":"upstream"},"usage":{"input_tokens":1,"output_tokens":1}}`},
		"end error":   {`{"type":"text","data":"partial"}`, `{"type":"end","stopReason":"error","usage":{"input_tokens":1,"output_tokens":1}}`},
	} {
		s, _ := NewStream(harness.Grok, nil, StreamOptions{})
		runGrokLines(t, s, "hi", lines...)
		r := s.Snapshot()
		if r.Failure == "" || len(r.Report) != 0 {
			t.Fatalf("%s: %+v", name, r)
		}
		if report, err := s.Report(); err == nil {
			t.Fatalf("%s: failed turn returned report %s", name, report)
		}
	}
}

func TestGrokSpendFlagsAndCostSources(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		lines                 []string
		usageKnown, costKnown bool
		costUSD               float64
	}{
		{"usage marked incomplete", []string{`{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"usage_is_incomplete":true,"total_cost_usd_ticks":10}`}, false, false, 1e-9},
		{"end without usage", []string{`{"type":"end","stopReason":"end_turn"}`}, false, false, 0},
		{"float cost only", []string{`{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"total_cost_usd":0.0279}`}, true, true, 0.0279},
		{"error spend with an end that has none", []string{`{"type":"error","message":"x","usage":{"input_tokens":7,"output_tokens":1},"total_cost_usd_ticks":5}`, `{"type":"end","stopReason":"end_turn"}`}, true, true, 5e-10},
		{"error spend with an incomplete end", []string{`{"type":"error","message":"x","usage":{"input_tokens":7,"output_tokens":1},"total_cost_usd_ticks":5}`, `{"type":"end","stopReason":"end_turn","usage_is_incomplete":true}`}, false, false, 5e-10},
	} {
		s, _ := NewStream(harness.Grok, nil, StreamOptions{})
		runGrokLines(t, s, "hi", append([]string{`{"type":"text","data":"a"}`}, tc.lines...)...)
		r := s.Snapshot()
		if r.Usage.Known != tc.usageKnown || r.Cost.Known != tc.costKnown || r.Cost.USD != tc.costUSD {
			t.Fatalf("%s: %+v", tc.name, r)
		}
	}
}

func TestGrokToolOutputShapes(t *testing.T) {
	var events []Event
	s, _ := NewStream(harness.Grok, nil, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
	runGrokLines(t, s, "hi",
		`{"type":"tool_call","toolCallId":"c","title":"Shell"}`,
		`{"type":"tool_call_update","toolCallId":"c","status":"in_progress","content":{"text":"halfway"}}`,
		`{"type":"tool_call_update","toolCallId":"c","status":"failed","rawOutput":{"exit":1}}`,
	)
	var got []string
	for _, e := range events {
		got = append(got, e.Kind+":"+e.ToolName+e.Output+strconv.FormatBool(e.Failed))
	}
	want := []string{"tool_start:Shellfalse", "tool_output:halfwayfalse", `tool_end:{"exit":1}true`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q want %q", got, want)
	}
}
