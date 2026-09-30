package completion

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func claudeResult(subtype string, isError bool, usage string) string {
	line := fmt.Sprintf(`{"type":"result","subtype":%q,"is_error":%v`, subtype, isError)
	if usage != "" {
		line += `,"usage":` + usage
	}
	if subtype == "success" && !isError {
		line += `,"structured_output":{"content":"done","tool_calls":[]}`
	}
	return line + "}"
}

func codexTurn(usage string) string {
	if usage == "" {
		return `{"type":"turn.completed"}`
	}
	return `{"type":"turn.completed","usage":` + usage + `}`
}

// One definition of what a stream establishes about consumption, whatever the
// invocation's outcome was.
func TestTerminalUsageIsReadFromAuthoritativeReportsOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine harness.Engine
		data   string
		want   harness.Usage
	}{
		// Claude states its cache split beside input_tokens; Input is every
		// prompt token, and the split is known only when both fields are there.
		{"claude error result", harness.Claude,
			claudeResult("error_during_execution", true, `{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":6,"cache_creation_input_tokens":2}`),
			harness.Usage{Known: true, Input: 18, Output: 4, CacheRead: 6, CacheWrite: 2, CacheKnown: true}},
		{"claude explicit zero cache split", harness.Claude,
			claudeResult("success", false, `{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`),
			harness.Usage{Known: true, Input: 10, Output: 4, CacheKnown: true}},
		{"claude cache fields absent", harness.Claude,
			claudeResult("error_during_execution", true, `{"input_tokens":10,"output_tokens":4}`),
			harness.Usage{Known: true, Input: 10, Output: 4}},
		{"claude one cache field absent", harness.Claude,
			claudeResult("success", false, `{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":6}`),
			harness.Usage{Known: true, Input: 16, Output: 4, CacheRead: 6}},
		// claude 2.1.285 states its thinking as a part of output_tokens; a figure
		// larger than its whole is not trusted as one.
		{"claude thinking", harness.Claude,
			claudeResult("success", false, `{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"thinking_tokens":3}}`),
			harness.Usage{Known: true, Input: 10, Output: 4, Reasoning: 3, ReasoningKnown: true}},
		{"claude thinking beyond output", harness.Claude,
			claudeResult("success", false, `{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"thinking_tokens":5}}`),
			harness.Usage{Known: true, Input: 10, Output: 4}},
		// An explicit zero is a measurement, and stays one.
		{"explicit zero is known", harness.Claude,
			claudeResult("success", false, `{"input_tokens":0,"output_tokens":0}`),
			harness.Usage{Known: true}},
		{"codex failed turn with completed accounting", harness.Codex,
			`{"type":"turn.failed","error":{"message":"stopped"}}` + "\n" + codexTurn(`{"input_tokens":30,"output_tokens":5}`),
			harness.Usage{Known: true, Input: 30, Output: 5}},
		// Codex's input_tokens already includes its cached input, and its
		// output_tokens its reasoning.
		{"codex cached input and reasoning", harness.Codex,
			codexTurn(`{"input_tokens":30,"cached_input_tokens":20,"output_tokens":5,"reasoning_output_tokens":2}`),
			harness.Usage{Known: true, Input: 30, Output: 5, CacheRead: 20, Reasoning: 2, CacheKnown: true, ReasoningKnown: true}},
		{"codex explicit zero cached input", harness.Codex,
			codexTurn(`{"input_tokens":30,"cached_input_tokens":0,"output_tokens":5}`),
			harness.Usage{Known: true, Input: 30, Output: 5, CacheKnown: true}},
		{"codex explicit zero is known", harness.Codex, codexTurn(`{"input_tokens":0,"output_tokens":0}`), harness.Usage{Known: true}},

		{"no report at all", harness.Claude, claudeResult("error_during_execution", true, ""), harness.Usage{}},
		{"empty report object", harness.Claude, claudeResult("success", false, `{}`), harness.Usage{}},
		{"missing output field", harness.Claude, claudeResult("success", false, `{"input_tokens":10}`), harness.Usage{}},
		{"negative input", harness.Claude, claudeResult("success", false, `{"input_tokens":-1,"output_tokens":4}`), harness.Usage{}},
		{"negative output", harness.Claude, claudeResult("success", false, `{"input_tokens":4,"output_tokens":-1}`), harness.Usage{}},
		{"negative cache field", harness.Claude, claudeResult("success", false, `{"input_tokens":4,"output_tokens":1,"cache_read_input_tokens":-2}`), harness.Usage{}},
		{"overflowing total", harness.Claude,
			claudeResult("success", false, fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d}`, int64(math.MaxInt64), 1)), harness.Usage{}},
		{"overflowing input", harness.Claude,
			claudeResult("success", false, fmt.Sprintf(`{"input_tokens":%d,"output_tokens":0,"cache_read_input_tokens":1}`, int64(math.MaxInt64))), harness.Usage{}},
		{"codex missing output", harness.Codex, codexTurn(`{"input_tokens":10}`), harness.Usage{}},
		{"codex cached exceeds input", harness.Codex, codexTurn(`{"input_tokens":10,"cached_input_tokens":11,"output_tokens":1}`), harness.Usage{}},
		{"codex negative cached input", harness.Codex, codexTurn(`{"input_tokens":10,"cached_input_tokens":-1,"output_tokens":1}`), harness.Usage{}},
		{"codex reasoning exceeds output", harness.Codex, codexTurn(`{"input_tokens":10,"output_tokens":1,"reasoning_output_tokens":2}`), harness.Usage{}},
		{"codex overflowing total", harness.Codex, codexTurn(fmt.Sprintf(`{"input_tokens":%d,"output_tokens":1}`, int64(math.MaxInt64))), harness.Usage{}},
		{"malformed stream", harness.Claude, "not json at all", harness.Usage{}},
		{"streamed estimate is not terminal", harness.Claude,
			`{"type":"assistant","usage":{"input_tokens":9,"output_tokens":9}}`, harness.Usage{}},

		// A second terminal makes the first ambiguous, including when it is the
		// second one that carries no accounting.
		{"duplicate terminal reports", harness.Claude,
			claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`) + "\n" + claudeResult("error_during_execution", true, `{"input_tokens":2,"output_tokens":2}`),
			harness.Usage{}},
		{"second terminal without usage", harness.Claude,
			claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`) + "\n" + claudeResult("error_during_execution", true, ""),
			harness.Usage{}},
		{"codex second terminal without usage", harness.Codex,
			codexTurn(`{"input_tokens":3,"output_tokens":1}`) + "\n" + codexTurn(""), harness.Usage{}},
		// A line we cannot read may itself be a terminal report, or may hide one.
		{"malformed line after a valid terminal", harness.Claude,
			claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`) + "\n" + `{"type":"result","usage":{"input_tok`,
			harness.Usage{}},
		{"truncated terminal data", harness.Codex,
			codexTurn(`{"input_tokens":3,"output_tokens":1}`) + "\n" + `{"type":"turn.compl`, harness.Usage{}},

		{"unknown engine", harness.OpenAICompatible, claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`), harness.Usage{}},
		{"no output at all", harness.Claude, "", harness.Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalAccounting(tc.engine, []byte(tc.data)).Usage; got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func claudeResultWithCost(subtype string, isError bool, usage, cost string) string {
	return strings.TrimSuffix(claudeResult(subtype, isError, usage), "}") + `,"total_cost_usd":` + cost + `}`
}

// Claude values its token spend on the single terminal result. Anything short
// of that is unknown, never free.
func TestClaudeCostIsTheTerminalResultsValuation(t *testing.T) {
	usage := `{"input_tokens":10,"output_tokens":4}`
	for _, tc := range []struct {
		name   string
		engine harness.Engine
		data   string
		want   harness.Cost
		known  bool
	}{
		{"success", harness.Claude, claudeResultWithCost("success", false, usage, "0.0123"), harness.Cost{USD: 0.0123, Known: true}, true},
		{"failed result is still valued", harness.Claude, claudeResultWithCost("error_during_execution", true, usage, "0.5"), harness.Cost{USD: 0.5, Known: true}, true},
		{"explicit zero is a valuation", harness.Claude, claudeResultWithCost("success", false, usage, "0"), harness.Cost{Known: true}, true},
		{"valued without usage", harness.Claude, claudeResultWithCost("success", false, "", "0.25"), harness.Cost{USD: 0.25, Known: true}, false},

		{"absent", harness.Claude, claudeResult("success", false, usage), harness.Cost{}, true},
		{"null", harness.Claude, claudeResultWithCost("success", false, usage, "null"), harness.Cost{}, true},
		{"negative", harness.Claude, claudeResultWithCost("success", false, usage, "-0.1"), harness.Cost{}, true},
		{"unreadable leaves usage known", harness.Claude, claudeResultWithCost("success", false, usage, `"0.1"`), harness.Cost{}, true},
		{"streamed estimate is not terminal", harness.Claude, `{"type":"assistant","total_cost_usd":0.1}`, harness.Cost{}, false},
		{"duplicate terminal reports", harness.Claude,
			claudeResultWithCost("success", false, usage, "0.1") + "\n" + claudeResultWithCost("error_during_execution", true, usage, "0.1"), harness.Cost{}, false},
		{"codex states no valuation", harness.Codex, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1},"total_cost_usd":0.1}`, harness.Cost{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := terminalAccounting(tc.engine, []byte(tc.data))
			if got.Cost != tc.want || got.Usage.Known != tc.known {
				t.Fatalf("got cost %+v usage %+v; want cost %+v known %v", got.Cost, got.Usage, tc.want, tc.known)
			}
		})
	}
}

// Success and failure must agree about the same stream's accounting: a report
// a failed invocation would reject cannot be accepted just because the
// invocation succeeded.
func TestSuccessAndFailurePathsAgreeOnUsage(t *testing.T) {
	sameAccounting := func(a, b Result) bool {
		return a.Usage == b.Usage && a.Cost == b.Cost && a.ContextWindow == b.ContextWindow
	}
	for _, tc := range []struct {
		name  string
		usage string
		known bool
	}{
		{"complete", `{"input_tokens":10,"output_tokens":2}`, true},
		{"with cache split", `{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":1,"cached_input_tokens":3}`, true},
		{"explicit zero", `{"input_tokens":0,"output_tokens":0}`, true},
		{"empty object", `{}`, false},
		{"missing output", `{"input_tokens":10}`, false},
		{"negative", `{"input_tokens":-3,"output_tokens":2}`, false},
		{"absent", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := parseClaude([]byte(claudeResult("success", false, tc.usage)), nil)
			if err != nil {
				t.Fatalf("success parse: %v", err)
			}
			failed, err := parseClaude([]byte(claudeResult("error_during_execution", true, tc.usage)), nil)
			if err == nil {
				t.Fatal("failed result parsed as success")
			}
			if ok.Usage.Known != tc.known || failed.Usage.Known != tc.known || !sameAccounting(ok, failed) {
				t.Fatalf("success %+v and failure %+v disagree (want known=%v)", ok, failed, tc.known)
			}

			envelope := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"done\",\"tool_calls\":[]}"}}`
			ok, err = parseCodex([]byte(envelope+"\n"+codexTurn(tc.usage)), nil)
			if err != nil {
				t.Fatalf("codex success parse: %v", err)
			}
			failed, err = parseCodex([]byte(`{"type":"turn.failed","error":{"message":"stopped"}}`+"\n"+codexTurn(tc.usage)), nil)
			if err == nil {
				t.Fatal("codex failed turn parsed as success")
			}
			if ok.Usage.Known != tc.known || failed.Usage.Known != tc.known || !sameAccounting(ok, failed) {
				t.Fatalf("codex success %+v and failure %+v disagree (want known=%v)", ok, failed, tc.known)
			}
		})
	}
}

// Parsing a failed stream must surface the failure and its accounting, and no
// action proposal.
func TestFailedParseKeepsUsageAndReturnsNoProposal(t *testing.T) {
	usage := `{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":0}`
	result, err := parseClaude([]byte(claudeResultWithCost("error_during_execution", true, usage, "0.02")), nil)
	if err == nil {
		t.Fatal("failed result parsed as success")
	}
	if used := result.Usage; !used.Known || used.Input != 17 || used.CacheRead != 5 || !used.CacheKnown || used.Output != 3 || used.Total() != 20 {
		t.Fatalf("terminal accounting lost: %+v", used)
	}
	if result.Cost != (harness.Cost{USD: 0.02, Known: true}) {
		t.Fatalf("terminal valuation lost: %+v", result.Cost)
	}
	if message := result.Message; message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatalf("failed request returned an action proposal: %+v", message)
	}

	failed := `{"type":"turn.failed","error":{"message":"stopped"}}` + "\n" + codexTurn(`{"input_tokens":7,"output_tokens":2}`)
	result, err = parseCodex([]byte(failed), nil)
	if err == nil {
		t.Fatal("failed turn parsed as success")
	}
	if used := result.Usage; !used.Known || used.Input != 7 || used.Total() != 9 {
		t.Fatalf("terminal accounting lost: %+v", used)
	}
	if message := result.Message; message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatalf("failed request returned an action proposal: %+v", message)
	}

	// An envelope that cannot be read is a response failure, and its partial
	// content is not a proposal either.
	bad := `{"type":"item.completed","item":{"type":"agent_message","text":"not an envelope"}}` + "\n" + codexTurn(`{"input_tokens":4,"output_tokens":1}`)
	result, err = parseCodex([]byte(bad), nil)
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Code != "invalid_action_envelope" {
		t.Fatalf("lost the envelope classification: %v", err)
	}
	if used := result.Usage; !used.Known || used.Total() != 5 {
		t.Fatalf("accounting lost on an unreadable envelope: %+v", used)
	}
	if message := result.Message; message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatalf("unreadable envelope returned a proposal: %+v", message)
	}
}

// A CLI that exits nonzero after the provider billed the request must not make
// that consumption disappear.
func TestProcessFailureKeepsReportedConsumption(t *testing.T) {
	root := t.TempDir()
	stream := claudeResultWithCost("error_during_execution", true, `{"input_tokens":40,"output_tokens":8,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, "0.75")
	cfg := Config{Provider: cliProvider(harness.Claude, "test-claude", filepath.Join(root, "login")), Model: "test-model", WorkDirRoot: root, MaxContextBytes: 100000, Timeout: time.Second}
	cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
		for _, entry := range env {
			if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
				return nil, answerClaudeProbe(args, strings.TrimPrefix(entry, "ANTHROPIC_BASE_URL="))
			}
		}
		return []byte(stream), errors.New("exit status 1")
	}
	result, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: "Go"}}, nil)
	if err == nil {
		t.Fatal("nonzero exit reported success")
	}
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Phase != PhaseProcess {
		t.Fatalf("lost the original process failure: %v", err)
	}
	if used := result.Usage; !used.Known || used.Input != 40 || used.Output != 8 || !used.CacheKnown {
		t.Fatalf("consumption reported by the provider was dropped: %+v", used)
	}
	if result.Cost != (harness.Cost{USD: 0.75, Known: true}) {
		t.Fatalf("valuation reported by the provider was dropped: %+v", result.Cost)
	}
	if result.Message.Content != "" || len(result.Message.ToolCalls) != 0 {
		t.Fatal("a failed process returned an action proposal")
	}
}

// Shapes as Claude 2.1.x stream-json emits them: the assistant message names
// the serving model, and the result's modelUsage keys each model it used.
func claudeAssistant(model string) string {
	return `{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":` + fmt.Sprintf("%q", model) + `,"content":[{"type":"text","text":"working"}],"usage":{"input_tokens":3,"output_tokens":1}},"session_id":"s"}`
}

func claudeModelUsage(entries map[string]string) string {
	parts := []string{}
	for model, window := range entries {
		entry := `{"inputTokens":10,"outputTokens":4,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.01,"maxOutputTokens":32000`
		if window != "" {
			entry += `,"contextWindow":` + window
		}
		parts = append(parts, fmt.Sprintf("%q:", model)+entry+"}")
	}
	return `{` + strings.Join(parts, ",") + `}`
}

func claudeResultWithModels(subtype string, isError bool, usage, modelUsage string) string {
	line := claudeResult(subtype, isError, usage)
	return strings.TrimSuffix(line, "}") + `,"modelUsage":` + modelUsage + `}`
}

func TestClaudeContextWindowIsTheServingModelsStatedWindow(t *testing.T) {
	usage := `{"input_tokens":10,"output_tokens":4}`
	both := claudeModelUsage(map[string]string{"claude-haiku-4-5": "200000", "claude-opus-4-7": "1000000"})
	for _, tc := range []struct {
		name  string
		data  string
		want  int64
		known bool
	}{
		{"named entry among several", claudeAssistant("claude-opus-4-7") + "\n" + claudeResultWithModels("success", false, usage, both), 1000000, true},
		{"latest assistant model wins", claudeAssistant("claude-opus-4-7") + "\n" + claudeAssistant("claude-haiku-4-5") + "\n" + claudeResultWithModels("success", false, usage, both), 200000, true},
		{"sole entry under a different key", claudeAssistant("opus") + "\n" + claudeResultWithModels("success", false, usage, claudeModelUsage(map[string]string{"claude-opus-4-7": "200000"})), 200000, true},
		{"sole entry after a synthetic message", claudeAssistant("<synthetic>") + "\n" + claudeResultWithModels("error_during_execution", true, usage, claudeModelUsage(map[string]string{"claude-opus-4-7": "200000"})), 200000, true},
		{"sole entry without assistant messages", claudeResultWithModels("success", false, usage, claudeModelUsage(map[string]string{"claude-opus-4-7": "200000"})), 200000, true},
		{"failed result with usage", claudeAssistant("claude-opus-4-7") + "\n" + claudeResultWithModels("error_during_execution", true, usage, both), 1000000, true},
		{"window without complete accounting", claudeResultWithModels("success", false, "", claudeModelUsage(map[string]string{"claude-opus-4-7": "200000"})), 200000, false},

		{"several entries without a match", claudeAssistant("opus") + "\n" + claudeResultWithModels("success", false, usage, both), 0, true},
		{"modelUsage absent", claudeAssistant("claude-opus-4-7") + "\n" + claudeResult("success", false, usage), 0, true},
		{"modelUsage empty", claudeResultWithModels("success", false, usage, `{}`), 0, true},
		{"entry without a window", claudeResultWithModels("success", false, usage, claudeModelUsage(map[string]string{"claude-opus-4-7": ""})), 0, true},
		{"zero window", claudeResultWithModels("success", false, usage, claudeModelUsage(map[string]string{"claude-opus-4-7": "0"})), 0, true},
		{"negative window", claudeResultWithModels("success", false, usage, claudeModelUsage(map[string]string{"claude-opus-4-7": "-1"})), 0, true},
		{"unreadable window", claudeResultWithModels("success", false, usage, `{"claude-opus-4-7":{"contextWindow":"large"}}`), 0, true},
		{"duplicate terminal reports", claudeResultWithModels("success", false, usage, both) + "\n" + claudeResultWithModels("success", false, usage, both), 0, false},
		{"window only in a streamed message", `{"type":"assistant","message":{"model":"claude-opus-4-7"},"modelUsage":` + both + `}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := terminalAccounting(harness.Claude, []byte(tc.data))
			if got.ContextWindow != tc.want || got.Usage.Known != tc.known {
				t.Fatalf("got %+v want window %d known %v", got, tc.want, tc.known)
			}
		})
	}
}

// Codex exec's JSON stream states no window; nothing in it may be read as one,
// and its differently shaped fields must not disturb its accounting.
func TestCodexContextWindowIsUnknown(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"t"}` + "\n" +
		`{"type":"error","message":"reconnecting"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"done\",\"tool_calls\":[]}"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":30,"cached_input_tokens":20,"output_tokens":5,"reasoning_output_tokens":2},"modelUsage":{"gpt-5":{"contextWindow":272000}}}`
	got := terminalAccounting(harness.Codex, []byte(stream))
	if got.Usage != (harness.Usage{Known: true, Input: 30, Output: 5, CacheRead: 20, Reasoning: 2, CacheKnown: true, ReasoningKnown: true}) || got.ContextWindow != 0 || got.Cost.Known {
		t.Fatalf("got %+v", got)
	}
}

func TestClaudeContextWindowSurvivesSuccessParseAndProcessFailure(t *testing.T) {
	stream := claudeAssistant("claude-opus-4-7") + "\n" + claudeResultWithModels("success", false, `{"input_tokens":10,"output_tokens":4}`, claudeModelUsage(map[string]string{"claude-opus-4-7": "200000"}))
	result, err := parseClaude([]byte(stream), nil)
	if err != nil || result.ContextWindow != 200000 || !result.Usage.Known {
		t.Fatalf("success parse: %+v %v", result, err)
	}
	result, err = parseClaude([]byte(strings.Replace(stream, `"subtype":"success","is_error":false`, `"subtype":"error_during_execution","is_error":true`, 1)), nil)
	if err == nil || result.ContextWindow != 200000 {
		t.Fatalf("failed parse: %+v %v", result, err)
	}

	root := t.TempDir()
	failed := claudeAssistant("claude-opus-4-7") + "\n" + claudeResultWithModels("error_during_execution", true, `{"input_tokens":40,"output_tokens":8}`, claudeModelUsage(map[string]string{"claude-opus-4-7": "1000000"}))
	cfg := Config{Provider: cliProvider(harness.Claude, "test-claude", filepath.Join(root, "login")), Model: "opus", WorkDirRoot: root, MaxContextBytes: 100000, Timeout: time.Second}
	cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
		for _, entry := range env {
			if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
				return nil, answerClaudeProbe(args, strings.TrimPrefix(entry, "ANTHROPIC_BASE_URL="))
			}
		}
		return []byte(failed), errors.New("exit status 1")
	}
	result, err = Complete(context.Background(), cfg, []Message{{Role: "user", Content: "Go"}}, nil)
	if err == nil || !result.Usage.Known || result.ContextWindow != 1000000 {
		t.Fatalf("process failure dropped the window: %+v %v", result, err)
	}
}
