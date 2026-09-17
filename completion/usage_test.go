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
		engine string
		data   string
		want   Usage
	}{
		{"claude error result", "claude",
			claudeResult("error_during_execution", true, `{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":6,"cache_creation_input_tokens":2}`),
			Usage{InputTokens: 18, OutputTokens: 4, TotalTokens: 22, Known: true}},
		{"claude cache fields absent", "claude",
			claudeResult("error_during_execution", true, `{"input_tokens":10,"output_tokens":4}`),
			Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14, Known: true}},
		// An explicit zero is a measurement, and stays one.
		{"explicit zero is known", "claude",
			claudeResult("success", false, `{"input_tokens":0,"output_tokens":0}`),
			Usage{Known: true}},
		{"codex failed turn with completed accounting", "codex",
			`{"type":"turn.failed","error":{"message":"stopped"}}` + "\n" + codexTurn(`{"input_tokens":30,"output_tokens":5}`),
			Usage{InputTokens: 30, OutputTokens: 5, TotalTokens: 35, Known: true}},
		{"codex explicit zero is known", "codex", codexTurn(`{"input_tokens":0,"output_tokens":0}`), Usage{Known: true}},

		{"no report at all", "claude", claudeResult("error_during_execution", true, ""), Usage{}},
		{"empty report object", "claude", claudeResult("success", false, `{}`), Usage{}},
		{"missing output field", "claude", claudeResult("success", false, `{"input_tokens":10}`), Usage{}},
		{"negative input", "claude", claudeResult("success", false, `{"input_tokens":-1,"output_tokens":4}`), Usage{}},
		{"negative output", "claude", claudeResult("success", false, `{"input_tokens":4,"output_tokens":-1}`), Usage{}},
		{"negative cache field", "claude", claudeResult("success", false, `{"input_tokens":4,"output_tokens":1,"cache_read_input_tokens":-2}`), Usage{}},
		{"overflowing total", "claude",
			claudeResult("success", false, fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d}`, math.MaxInt, 1)), Usage{}},
		{"codex missing output", "codex", codexTurn(`{"input_tokens":10}`), Usage{}},
		{"malformed stream", "claude", "not json at all", Usage{}},
		{"streamed estimate is not terminal", "claude",
			`{"type":"assistant","usage":{"input_tokens":9,"output_tokens":9}}`, Usage{}},

		// A second terminal makes the first ambiguous, including when it is the
		// second one that carries no accounting.
		{"duplicate terminal reports", "claude",
			claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`) + "\n" + claudeResult("error_during_execution", true, `{"input_tokens":2,"output_tokens":2}`),
			Usage{}},
		{"second terminal without usage", "claude",
			claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`) + "\n" + claudeResult("error_during_execution", true, ""),
			Usage{}},
		{"codex second terminal without usage", "codex",
			codexTurn(`{"input_tokens":3,"output_tokens":1}`) + "\n" + codexTurn(""), Usage{}},
		// A line we cannot read may itself be a terminal report, or may hide one.
		{"malformed line after a valid terminal", "claude",
			claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`) + "\n" + `{"type":"result","usage":{"input_tok`,
			Usage{}},
		{"truncated terminal data", "codex",
			codexTurn(`{"input_tokens":3,"output_tokens":1}`) + "\n" + `{"type":"turn.compl`, Usage{}},

		{"unknown engine", "openai-compatible", claudeResult("success", false, `{"input_tokens":1,"output_tokens":1}`), Usage{}},
		{"no output at all", "claude", "", Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalUsage(tc.engine, []byte(tc.data)); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// Success and failure must agree about the same stream's accounting: a report
// a failed invocation would reject cannot be accepted just because the
// invocation succeeded.
func TestSuccessAndFailurePathsAgreeOnUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
		known bool
	}{
		{"complete", `{"input_tokens":10,"output_tokens":2}`, true},
		{"explicit zero", `{"input_tokens":0,"output_tokens":0}`, true},
		{"empty object", `{}`, false},
		{"missing output", `{"input_tokens":10}`, false},
		{"negative", `{"input_tokens":-3,"output_tokens":2}`, false},
		{"absent", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := parseClaude([]byte(claudeResult("success", false, tc.usage)), nil)
			if err != nil {
				t.Fatalf("success parse: %v", err)
			}
			_, failed, err := parseClaude([]byte(claudeResult("error_during_execution", true, tc.usage)), nil)
			if err == nil {
				t.Fatal("failed result parsed as success")
			}
			if ok.Known != tc.known || failed.Known != tc.known || ok != failed {
				t.Fatalf("success %+v and failure %+v disagree (want known=%v)", ok, failed, tc.known)
			}

			envelope := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"done\",\"tool_calls\":[]}"}}`
			_, ok, err = parseCodex([]byte(envelope+"\n"+codexTurn(tc.usage)), nil)
			if err != nil {
				t.Fatalf("codex success parse: %v", err)
			}
			_, failed, err = parseCodex([]byte(`{"type":"turn.failed","error":{"message":"stopped"}}`+"\n"+codexTurn(tc.usage)), nil)
			if err == nil {
				t.Fatal("codex failed turn parsed as success")
			}
			if ok.Known != tc.known || failed.Known != tc.known || ok != failed {
				t.Fatalf("codex success %+v and failure %+v disagree (want known=%v)", ok, failed, tc.known)
			}
		})
	}
}

// Parsing a failed stream must surface the failure and its accounting, and no
// action proposal.
func TestFailedParseKeepsUsageAndReturnsNoProposal(t *testing.T) {
	usage := `{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":0}`
	message, used, err := parseClaude([]byte(claudeResult("error_during_execution", true, usage)), nil)
	if err == nil {
		t.Fatal("failed result parsed as success")
	}
	if !used.Known || used.InputTokens != 17 || used.OutputTokens != 3 || used.TotalTokens != 20 {
		t.Fatalf("terminal accounting lost: %+v", used)
	}
	if message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatalf("failed request returned an action proposal: %+v", message)
	}

	failed := `{"type":"turn.failed","error":{"message":"stopped"}}` + "\n" + codexTurn(`{"input_tokens":7,"output_tokens":2}`)
	message, used, err = parseCodex([]byte(failed), nil)
	if err == nil {
		t.Fatal("failed turn parsed as success")
	}
	if !used.Known || used.InputTokens != 7 || used.TotalTokens != 9 {
		t.Fatalf("terminal accounting lost: %+v", used)
	}
	if message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatalf("failed request returned an action proposal: %+v", message)
	}

	// An envelope that cannot be read is a response failure, and its partial
	// content is not a proposal either.
	bad := `{"type":"item.completed","item":{"type":"agent_message","text":"not an envelope"}}` + "\n" + codexTurn(`{"input_tokens":4,"output_tokens":1}`)
	message, used, err = parseCodex([]byte(bad), nil)
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Code != "invalid_action_envelope" {
		t.Fatalf("lost the envelope classification: %v", err)
	}
	if !used.Known || used.TotalTokens != 5 {
		t.Fatalf("accounting lost on an unreadable envelope: %+v", used)
	}
	if message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatalf("unreadable envelope returned a proposal: %+v", message)
	}
}

// A CLI that exits nonzero after the provider billed the request must not make
// that consumption disappear.
func TestProcessFailureKeepsReportedConsumption(t *testing.T) {
	root := t.TempDir()
	stream := claudeResult("error_during_execution", true, `{"input_tokens":40,"output_tokens":8,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`)
	cfg := Config{Engine: "claude", ClaudeHome: filepath.Join(root, "login"), ClaudeBin: "test-claude", Model: "test-model", WorkDirRoot: root, MaxContextBytes: 100000, Timeout: time.Second}
	cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
		for _, entry := range env {
			if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
				return nil, answerClaudeProbe(args, strings.TrimPrefix(entry, "ANTHROPIC_BASE_URL="))
			}
		}
		return []byte(stream), errors.New("exit status 1")
	}
	message, used, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: "Go"}}, nil)
	if err == nil {
		t.Fatal("nonzero exit reported success")
	}
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Phase != PhaseProcess {
		t.Fatalf("lost the original process failure: %v", err)
	}
	if !used.Known || used.InputTokens != 40 || used.OutputTokens != 8 {
		t.Fatalf("consumption reported by the provider was dropped: %+v", used)
	}
	if message.Content != "" || len(message.ToolCalls) != 0 {
		t.Fatal("a failed process returned an action proposal")
	}
}
