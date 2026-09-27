//go:build !windows

package completion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGrokCarriesSkillsInItsProvenPayload(t *testing.T) {
	s := newGrokSetup(t)
	s.cfg.Skills = skillSet(t)
	s.fake.stream = []string{grokCatalog, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"structuredOutput":{"content":"","tool_calls":[{"name":"load_skill","arguments":"{\"skill\":\"guide\"}"},{"name":"read_state","arguments":"{}"}]}}`}
	result, err := Complete(context.Background(), s.cfg, userMessage, Tools())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SkillCalls) != 1 || result.SkillCalls[0].Function.Name != LoadSkillTool || len(result.ApplicationCalls()) != 1 {
		t.Fatalf("result %+v", result)
	}
	var payload cliPayload
	if err := json.Unmarshal([]byte(s.fake.prompt), &payload); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(payload.Tools), ","); got != "read_state,load_skill,run_skill_script" {
		t.Fatalf("available_tools %s", got)
	}
	if payload.Messages[0].Role != "system" || !strings.Contains(payload.Messages[0].Content, "- guide: Guides guide work.") {
		t.Fatalf("messages %+v", payload.Messages)
	}
	if flagValues(s.fake.args)["system-prompt-override"] != codexInstructions {
		t.Fatal("skills changed the proven system instructions")
	}
}

func TestLibraryRunnerAnswersScriptCalls(t *testing.T) {
	set := skillSet(t)
	work := t.TempDir()
	answers := AnswerSkillCalls(context.Background(), []ToolCall{toolCall("r", RunSkillScriptTool, `{"skill":"tools","script":"run.sh","args":["$(touch pwned)"]}`)}, set, SkillRunOptions{WorkDir: work})
	var output SkillOutput
	if len(answers) != 1 || json.Unmarshal([]byte(answers[0].Content), &output) != nil || output.ExitCode != 0 || output.Stdout != "ran $(touch pwned)\n" {
		t.Fatalf("answers %+v", answers)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	answers = AnswerSkillCalls(ctx, []ToolCall{toolCall("r", RunSkillScriptTool, `{"skill":"tools","script":"run.sh"}`)}, set, SkillRunOptions{WorkDir: work})
	if answers[0].Content != "run_skill_script error: cancelled" {
		t.Fatalf("a cancelled answer ran: %q", answers[0].Content)
	}
}
