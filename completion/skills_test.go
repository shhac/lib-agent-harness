package completion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// testSkill writes a synthetic skill directory: SKILL.md, a reference file
// and, when given, other files.
func testSkill(t *testing.T, name string, scripts bool, files map[string]string) harness.Skill {
	t.Helper()
	dir := t.TempDir()
	all := map[string]string{
		"SKILL.md":     "---\nname: " + name + "\ndescription: Guides " + name + " work.\n---\n# " + name + "\nRead reference.md.\n",
		"reference.md": "reference for " + name,
	}
	for rel, content := range files {
		all[rel] = content
	}
	for rel, content := range all {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return harness.Skill{Name: name, Dir: dir, Scripts: scripts}
}

func skillSet(t *testing.T) harness.Skills {
	return harness.Skills{Provided: []harness.Skill{testSkill(t, "guide", false, nil), testSkill(t, "tools", true, map[string]string{"run.sh": "#!/bin/sh\necho ran \"$@\"\n"})}}
}

func toolCall(id, name, arguments string) ToolCall {
	call := ToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, arguments
	return call
}

// sequenceAPI answers each request with the next body.
func sequenceAPI(bodies ...string) *fakeAPI {
	api := &fakeAPI{}
	api.respond = func(r *http.Request) (*http.Response, error) {
		body := bodies[min(api.count(), len(bodies))-1]
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	return api
}

type wireRequest struct {
	Messages []struct {
		Role       string     `json:"role"`
		Content    *string    `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls"`
		ToolCallID string     `json:"tool_call_id"`
	} `json:"messages"`
	Tools []Tool `json:"tools"`
}

func decodeWire(t *testing.T, body []byte) wireRequest {
	t.Helper()
	var request wireRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

// requireToolPairing applies Chat Completions' rule: an assistant message
// with tool calls is followed by exactly one tool message per call, and a
// tool message answers a call of the assistant message before it.
func requireToolPairing(t *testing.T, request wireRequest) {
	t.Helper()
	pending := map[string]bool{}
	for i, message := range request.Messages {
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				t.Fatalf("message %d answers no pending call: %q", i, message.ToolCallID)
			}
			delete(pending, message.ToolCallID)
			continue
		}
		if len(pending) != 0 {
			t.Fatalf("message %d follows unanswered calls %v", i, pending)
		}
		for _, call := range message.ToolCalls {
			pending[call.ID] = true
		}
	}
	if len(pending) != 0 {
		t.Fatalf("unanswered calls %v", pending)
	}
}

func toolNames(tools []Tool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func enumOf(t *testing.T, tool Tool) []any {
	t.Helper()
	properties, _ := tool.Function.Parameters["properties"].(map[string]any)
	skill, _ := properties["skill"].(map[string]any)
	values, _ := skill["enum"].([]any)
	return values
}

func TestSkillsReachTheOpenAIRequest(t *testing.T) {
	set := skillSet(t)
	api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"ok"}`)))
	cfg := apiConfig(api)
	cfg.Skills = set
	caller := []Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "Go."}}
	if _, err := Complete(context.Background(), cfg, caller, lookupTool); err != nil {
		t.Fatal(err)
	}
	if caller[0].Content != "Be brief." {
		t.Fatal("the caller's messages were changed")
	}
	request := decodeWire(t, api.last(t).body)
	if len(request.Messages) != 2 || request.Messages[0].Role != "system" {
		t.Fatalf("messages %+v", request.Messages)
	}
	system := *request.Messages[0].Content
	for _, want := range []string{"Be brief.\n\nSkills are available through the load_skill function.", "\n- guide: Guides guide work.", "\n- tools (scripts): Guides tools work.", "run_skill_script"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system context lacks %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, set.Provided[0].Dir) {
		t.Fatal("the index revealed a skill directory")
	}
	if got := strings.Join(toolNames(request.Tools), ","); got != "lookup,load_skill,run_skill_script" {
		t.Fatalf("tools %s", got)
	}
	if got := enumOf(t, request.Tools[1]); len(got) != 2 || got[0] != "guide" || got[1] != "tools" {
		t.Fatalf("load_skill enum %v", got)
	}
	if got := enumOf(t, request.Tools[2]); len(got) != 1 || got[0] != "tools" {
		t.Fatalf("run_skill_script enum %v", got)
	}

	// Without a skill that permits scripts there is no script tool, and
	// without a leading system message the index is one.
	api = respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"ok"}`)))
	cfg = apiConfig(api)
	cfg.Skills = harness.Skills{Global: harness.GlobalSkillsExclude, Delivery: harness.SkillDeliveryComposed, Provided: set.Provided[:1]}
	if _, err := Complete(context.Background(), cfg, userMessage, nil); err != nil {
		t.Fatal(err)
	}
	request = decodeWire(t, api.last(t).body)
	if got := strings.Join(toolNames(request.Tools), ","); got != "load_skill" {
		t.Fatalf("tools %s", got)
	}
	if request.Messages[0].Role != "system" || strings.Contains(*request.Messages[0].Content, "run_skill_script") || request.Messages[1].Role != "user" {
		t.Fatalf("messages %+v", request.Messages)
	}
}

func TestSkillCallsRoundTripOverOpenAI(t *testing.T) {
	set := skillSet(t)
	api := sequenceAPI(
		chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(
			chatCall("call_s", "load_skill", `{"skill":"guide"}`),
			chatCall("call_a", "lookup", `{"q":"x"}`),
			chatCall("call_r", "run_skill_script", `{"skill":"tools","script":"run.sh","args":["a b","$(x)"]}`),
		))),
		chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"done"}`)),
	)
	cfg := apiConfig(api)
	cfg.Skills = set
	history := []Message{{Role: "user", Content: "Go."}}
	result, err := Complete(context.Background(), cfg, history, lookupTool)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Message.ToolCalls) != 3 || len(result.SkillCalls) != 2 || result.SkillCalls[0].ID != "call_s" || result.SkillCalls[1].ID != "call_r" {
		t.Fatalf("result %+v", result)
	}
	if application := result.ApplicationCalls(); len(application) != 1 || application[0].ID != "call_a" {
		t.Fatalf("application calls %+v", application)
	}

	work := t.TempDir()
	var ran SkillCommand
	answers := AnswerSkillCalls(context.Background(), result.Message.ToolCalls, set, SkillRunOptions{WorkDir: work, Env: []string{"APP_VALUE=1"}, Exec: func(_ context.Context, command SkillCommand) (SkillOutput, error) {
		ran = command
		return SkillOutput{ExitCode: 0, Stdout: "hook ran " + strings.Join(command.Args, "|")}, nil
	}})
	if len(answers) != 2 || answers[0].ToolCallID != "call_s" || answers[1].ToolCallID != "call_r" {
		t.Fatalf("answers %+v", answers)
	}
	if !strings.HasPrefix(answers[0].Content, "---\nname: guide") {
		t.Fatalf("load_skill answer %q", answers[0].Content)
	}
	var output SkillOutput
	if err := json.Unmarshal([]byte(answers[1].Content), &output); err != nil || output.Stdout != "hook ran a b|$(x)" || output.ExitCode != 0 {
		t.Fatalf("script answer %q", answers[1].Content)
	}
	root, _ := filepath.EvalSymlinks(set.Provided[1].Dir)
	if ran.Skill != "tools" || ran.Dir != root || ran.Script != filepath.Join(root, "run.sh") || ran.WorkDir != work || ran.Timeout <= 0 {
		t.Fatalf("command %+v", ran)
	}
	if !contains(ran.Env, "APP_VALUE=1") {
		t.Fatalf("environment %v", ran.Env)
	}

	history = append(history, result.Message)
	history = append(history, answers...)
	history = append(history, Message{Role: "tool", ToolCallID: "call_a", Content: `{"found":true}`})
	final, err := Complete(context.Background(), cfg, history, lookupTool)
	if err != nil || final.Message.Content != "done" || final.SkillCalls != nil {
		t.Fatalf("%+v %v", final, err)
	}
	api.mu.Lock()
	second := decodeWire(t, api.requests[1].body)
	api.mu.Unlock()
	requireToolPairing(t, second)
	if len(second.Messages) != 6 || len(second.Messages[2].ToolCalls) != 3 {
		t.Fatalf("history %+v", second.Messages)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestAnswerSkillCallsRefusesWithFixedText(t *testing.T) {
	set := skillSet(t)
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "key.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	failing := func(context.Context, SkillCommand) (SkillOutput, error) {
		return SkillOutput{}, errors.New("secret sandbox failure")
	}
	work := t.TempDir()
	for name, tc := range map[string]struct {
		call ToolCall
		opts SkillRunOptions
		want string
	}{
		"reference file":        {toolCall("1", LoadSkillTool, `{"skill":"guide","file":"reference.md"}`), SkillRunOptions{}, "reference for guide"},
		"unknown skill":         {toolCall("1", LoadSkillTool, `{"skill":"other"}`), SkillRunOptions{}, "load_skill error: unknown_skill"},
		"escape":                {toolCall("1", LoadSkillTool, `{"skill":"guide","file":"../`+filepath.Base(secret)+`/key.txt"}`), SkillRunOptions{}, "load_skill error: file_outside_skill"},
		"absolute":              {toolCall("1", LoadSkillTool, `{"skill":"guide","file":"`+filepath.ToSlash(filepath.Join(secret, "key.txt"))+`"}`), SkillRunOptions{}, "load_skill error: file_path_invalid"},
		"missing":               {toolCall("1", LoadSkillTool, `{"skill":"guide","file":"none.md"}`), SkillRunOptions{}, "load_skill error: file_not_found"},
		"extra field":           {toolCall("1", LoadSkillTool, `{"skill":"guide","path":"x"}`), SkillRunOptions{}, "load_skill error: invalid_arguments"},
		"not an object":         {toolCall("1", LoadSkillTool, `"guide"`), SkillRunOptions{}, "load_skill error: invalid_arguments"},
		"scripts not permitted": {toolCall("1", RunSkillScriptTool, `{"skill":"guide","script":"reference.md"}`), SkillRunOptions{WorkDir: work, Exec: failing}, "run_skill_script error: scripts_not_permitted"},
		"no work dir":           {toolCall("1", RunSkillScriptTool, `{"skill":"tools","script":"run.sh"}`), SkillRunOptions{Exec: failing}, "run_skill_script error: work_dir_required"},
		"script escape":         {toolCall("1", RunSkillScriptTool, `{"skill":"tools","script":"../run.sh"}`), SkillRunOptions{WorkDir: work, Exec: failing}, "run_skill_script error: file_outside_skill"},
		"args not strings":      {toolCall("1", RunSkillScriptTool, `{"skill":"tools","script":"run.sh","args":[1]}`), SkillRunOptions{WorkDir: work, Exec: failing}, "run_skill_script error: invalid_arguments"},
		"hook failure":          {toolCall("1", RunSkillScriptTool, `{"skill":"tools","script":"run.sh"}`), SkillRunOptions{WorkDir: work, Exec: failing}, "run_skill_script error: script_failed"},
		"bad timeout":           {toolCall("1", RunSkillScriptTool, `{"skill":"tools","script":"run.sh"}`), SkillRunOptions{WorkDir: work, Timeout: -1, Exec: failing}, "run_skill_script error: script_timeout_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			answers := AnswerSkillCalls(context.Background(), []ToolCall{toolCall("app", "lookup", `{}`), tc.call}, set, tc.opts)
			if len(answers) != 1 || answers[0].Role != "tool" || answers[0].ToolCallID != "1" || answers[0].Content != tc.want {
				t.Fatalf("answers %+v", answers)
			}
		})
	}

	broken := harness.Skills{Provided: []harness.Skill{{Name: "guide", Dir: "relative"}}}
	answers := AnswerSkillCalls(context.Background(), []ToolCall{toolCall("1", LoadSkillTool, `{"skill":"guide"}`)}, broken, SkillRunOptions{})
	if len(answers) != 1 || answers[0].Content != "load_skill error: skills_unavailable" {
		t.Fatalf("answers %+v", answers)
	}

	loud := func(context.Context, SkillCommand) (SkillOutput, error) {
		return SkillOutput{Stdout: strings.Repeat("o", 100<<10), Stderr: "e"}, nil
	}
	answers = AnswerSkillCalls(context.Background(), []ToolCall{toolCall("1", RunSkillScriptTool, `{"skill":"tools","script":"run.sh"}`)}, set, SkillRunOptions{WorkDir: work, Exec: loud})
	var output SkillOutput
	if err := json.Unmarshal([]byte(answers[0].Content), &output); err != nil || len(output.Stdout) != 64<<10 || !output.StdoutTruncated || output.Stderr != "e" || output.StderrTruncated {
		t.Fatalf("a hook's output was not bounded: %d %v", len(output.Stdout), err)
	}
}

func TestMalformedSkillCallsFailTheResponse(t *testing.T) {
	for _, call := range []string{
		chatCall("c", "load_skill", `{"skill":"guide","extra":true}`),
		chatCall("c", "load_skill", `{"skill":5}`),
		chatCall("c", "load_skill", `{"file":"reference.md"}`),
		chatCall("c", "load_skill", `{"skill":"guide","file":null}`),
		chatCall("c", "load_skill", `{"skill":"guide","file":""}`),
		chatCall("c", "run_skill_script", `{"skill":"tools"}`),
		chatCall("c", "run_skill_script", `{"skill":"tools","script":"run.sh","args":"a"}`),
	} {
		api := respondWith(200, chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(call))))
		cfg := apiConfig(api)
		cfg.Skills = skillSet(t)
		result, err := Complete(context.Background(), cfg, userMessage, nil)
		requireAPIFailure(t, err, PhaseResponse, harness.CauseUnknown, "invalid_skill_call")
		if result.Message.ToolCalls != nil || result.SkillCalls != nil || !result.Usage.Known {
			t.Fatalf("%s: %+v", call, result)
		}
	}
}

func TestSkillRequestsThatCannotBeHonouredAreRefused(t *testing.T) {
	set := skillSet(t)
	reserved := func(name string) []Tool { return []Tool{{Type: "function", Function: Function{Name: name}}} }
	for name, tc := range map[string]struct {
		skills harness.Skills
		tools  []Tool
		code   string
		family harness.Family
	}{
		"include global":   {harness.Skills{Global: harness.GlobalSkillsInclude}, nil, "global_skills_unsupported", harness.FailureCapability},
		"unknown global":   {harness.Skills{Global: "all"}, nil, "global_skills_invalid", harness.FailurePreflight},
		"unknown delivery": {harness.Skills{Delivery: "native"}, nil, "skill_delivery_invalid", harness.FailurePreflight},
		"reserved load":    {set, reserved(LoadSkillTool), "skill_tool_name_reserved", harness.FailurePreflight},
		"reserved script":  {set, reserved(RunSkillScriptTool), "skill_tool_name_reserved", harness.FailurePreflight},
		"relative dir":     {harness.Skills{Provided: []harness.Skill{{Name: "guide", Dir: "guide"}}}, nil, "skill_dir_invalid", harness.FailurePreflight},
		"name mismatch":    {harness.Skills{Provided: []harness.Skill{{Name: "other", Dir: set.Provided[0].Dir}}}, nil, "skill_name_mismatch", harness.FailurePreflight},
		"duplicate":        {harness.Skills{Provided: []harness.Skill{set.Provided[0], set.Provided[0]}}, nil, "skill_duplicate", harness.FailurePreflight},
		"missing manifest": {harness.Skills{Provided: []harness.Skill{{Name: "guide", Dir: t.TempDir()}}}, nil, "skill_manifest_missing", harness.FailurePreflight},
	} {
		t.Run(name, func(t *testing.T) {
			for _, engine := range harness.Engines() {
				if !harness.Support(engine, harness.Complete, harness.Available).Usable() {
					continue
				}
				api := respondWith(200, chatBody(chatChoice(`"stop"`, `{"role":"assistant","content":"ok"}`)))
				cfg := apiConfig(api)
				if engine != harness.OpenAICompatible {
					cfg = Config{Provider: cliProvider(engine, "harness-test-not-installed", ""), Model: "m", run: func(context.Context, string, []string, string, []string, string) ([]byte, error) {
						t.Fatal("a refused request launched a CLI")
						return nil, nil
					}}
				}
				cfg.Skills = tc.skills
				_, err := Complete(context.Background(), cfg, userMessage, tc.tools)
				failure := requireDiagnostic(t, err, engine, PhasePreflight, tc.code)
				facts, ok := harness.ErrorFacts(err)
				if !ok || facts != failure.HarnessFacts() || facts.Family != tc.family || facts.Operation != harness.Complete || facts.Engine != engine || facts.Retryable {
					t.Fatalf("%s facts %+v", engine, facts)
				}
				if api.count() != 0 {
					t.Fatal("a refused request reached the endpoint")
				}
			}
		})
	}

	// Without provided skills the names are the caller's own.
	api := respondWith(200, chatBody(chatChoice(`"tool_calls"`, assistantWithCalls(chatCall("c", LoadSkillTool, `{"anything":1}`)))))
	result, err := Complete(context.Background(), apiConfig(api), userMessage, reserved(LoadSkillTool))
	if err != nil || len(result.Message.ToolCalls) != 1 || result.SkillCalls != nil || len(result.ApplicationCalls()) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestSkillsSupportIsComposedForCompletion(t *testing.T) {
	for _, engine := range harness.Engines() {
		if !harness.Support(engine, harness.Complete, harness.Available).Usable() {
			continue
		}
		if c := harness.Support(engine, harness.Complete, harness.ProvidedSkills); c.Availability != harness.Composed || c.Reason == "" {
			t.Errorf("%s skills %+v", engine, c)
		}
		if c := harness.Support(engine, harness.Complete, harness.IncludeGlobalSkills); c.Usable() {
			t.Errorf("%s global skills %+v", engine, c)
		}
	}
}

// cliSkillConfig answers a CLI engine's probes and returns the replies in
// turn, recording each real payload.
func cliSkillConfig(t *testing.T, engine harness.Engine, payloads *[]string, schemas *[]string, replies ...map[string]any) Config {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: cliProvider(engine, binary, t.TempDir()), Model: "test-model", Effort: "high", WorkDirRoot: t.TempDir()}
	cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, input string) ([]byte, error) {
		if args[0] == "debug" {
			return []byte(testCatalog), nil
		}
		if url := findProbeURL(args); url != "" {
			postProbe(t, url, `{"model":"test-model","reasoning":{"effort":"high"},"tools":[]}`)
			return nil, errors.New("intentional local probe rejection")
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
				return nil, answerClaudeProbe(args, strings.TrimPrefix(entry, "ANTHROPIC_BASE_URL="))
			}
		}
		for i, arg := range args {
			if arg == "--json-schema" {
				*schemas = append(*schemas, args[i+1])
			}
		}
		*payloads = append(*payloads, input)
		action := replies[len(*payloads)-1]
		if engine == harness.Claude {
			return json.Marshal(map[string]any{"type": "result", "subtype": "success", "structured_output": action})
		}
		raw, _ := json.Marshal(action)
		event, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": string(raw)}})
		return append(event, []byte("\n{\"type\":\"turn.completed\"}\n")...), nil
	}
	return cfg
}

type cliPayload struct {
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"available_tools"`
}

func TestSkillCallsRoundTripOverCLIEngines(t *testing.T) {
	testenv.RequireLoopback(t) // Constrained CLI preflight starts a local refusal provider.
	for _, engine := range cliEngines {
		t.Run(string(engine), func(t *testing.T) {
			set := skillSet(t)
			var payloads, schemas []string
			propose := map[string]any{"content": "", "tool_calls": []any{
				map[string]string{"name": "load_skill", "arguments": `{"skill":"tools","file":"reference.md"}`},
				map[string]string{"name": "run_skill_script", "arguments": `{"skill":"tools","script":"run.sh","args":["x"]}`},
			}}
			cfg := cliSkillConfig(t, engine, &payloads, &schemas, propose, map[string]any{"content": "done", "tool_calls": []any{}})
			cfg.Skills = set
			history := []Message{{Role: "user", Content: "Go."}}
			result, err := Complete(context.Background(), cfg, history, Tools())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.SkillCalls) != 2 || len(result.Message.ToolCalls) != 2 || result.ApplicationCalls() != nil {
				t.Fatalf("result %+v", result)
			}
			var payload cliPayload
			if err := json.Unmarshal([]byte(payloads[0]), &payload); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(toolNames(payload.Tools), ","); got != "read_state,load_skill,run_skill_script" {
				t.Fatalf("available_tools %s", got)
			}
			if payload.Messages[0].Role != "system" || !strings.Contains(payload.Messages[0].Content, "- tools (scripts): Guides tools work.") {
				t.Fatalf("messages %+v", payload.Messages)
			}
			if engine == harness.Claude && (len(schemas) == 0 || !strings.Contains(schemas[0], `"load_skill"`) || !strings.Contains(schemas[0], `"run_skill_script"`)) {
				t.Fatalf("the action schema does not admit the skill tools: %v", schemas)
			}

			answers := AnswerSkillCalls(context.Background(), result.SkillCalls, set, SkillRunOptions{WorkDir: t.TempDir(), Exec: func(_ context.Context, command SkillCommand) (SkillOutput, error) {
				return SkillOutput{Stdout: "ran " + strings.Join(command.Args, " ")}, nil
			}})
			if len(answers) != 2 || answers[0].Content != "reference for tools" || !strings.Contains(answers[1].Content, `"stdout":"ran x"`) {
				t.Fatalf("answers %+v", answers)
			}
			history = append(append(history, result.Message), answers...)
			final, err := Complete(context.Background(), cfg, history, Tools())
			if err != nil || final.Message.Content != "done" {
				t.Fatalf("%+v %v", final, err)
			}
			if err := json.Unmarshal([]byte(payloads[1]), &payload); err != nil {
				t.Fatal(err)
			}
			messages := payload.Messages
			if len(messages) != 5 || messages[2].Role != "assistant" || messages[3].ToolCallID != messages[2].ToolCalls[0].ID || messages[4].ToolCallID != messages[2].ToolCalls[1].ID {
				t.Fatalf("history %+v", messages)
			}
		})
	}
}
