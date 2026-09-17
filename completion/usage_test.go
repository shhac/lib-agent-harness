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

// A provider that rejected or abandoned a request can still have charged for
// it. Its own terminal accounting is the only honest record of that.
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
		{"codex failed turn with completed accounting", "codex",
			`{"type":"turn.failed","error":{"message":"stopped"}}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":30,"output_tokens":5}}`,
			Usage{InputTokens: 30, OutputTokens: 5, TotalTokens: 35, Known: true}},
		{"no report at all", "claude", claudeResult("error_during_execution", true, ""), Usage{}},
		{"missing output field", "claude", claudeResult("error_during_execution", true, `{"input_tokens":10}`), Usage{}},
		{"negative field", "claude", claudeResult("error_during_execution", true, `{"input_tokens":-1,"output_tokens":4}`), Usage{}},
		{"overflowing total", "claude",
			claudeResult("error_during_execution", true, fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d}`, math.MaxInt, 1)), Usage{}},
		{"malformed stream", "claude", "not json at all", Usage{}},
		{"streamed estimate is not terminal", "claude",
			`{"type":"assistant","usage":{"input_tokens":9,"output_tokens":9}}`, Usage{}},
		{"two terminal reports are not authoritative", "claude",
			claudeResult("error_during_execution", true, `{"input_tokens":1,"output_tokens":1}`) + "\n" + claudeResult("error_during_execution", true, `{"input_tokens":2,"output_tokens":2}`),
			Usage{}},
		{"unknown engine", "openai-compatible", claudeResult("error_during_execution", true, `{"input_tokens":1,"output_tokens":1}`), Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TerminalUsage(tc.engine, []byte(tc.data))
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
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

	failed := `{"type":"turn.failed","error":{"message":"stopped"}}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":2}}`
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

// Output with no usable accounting stays unknown after a process failure, so a
// caller can tell "not reported" from "reported zero".
func TestProcessFailureWithoutAccountingStaysUnknown(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Engine: "codex", CodexHome: filepath.Join(root, "login"), CodexBin: "test-codex", Model: "test-model", WorkDirRoot: root, MaxContextBytes: 100000, Timeout: time.Second}
	if used := TerminalUsage("codex", []byte("partial output, killed")); used.Known {
		t.Fatalf("fabricated accounting from unusable output: %+v", used)
	}
	if used := TerminalUsage(cfg.Engine, nil); used.Known {
		t.Fatalf("fabricated accounting from no output: %+v", used)
	}
}
