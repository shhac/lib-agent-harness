package completion

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

// grokContextText is the vendor context message Grok 1.0.41 sends before the
// prompt, with synthetic built-in rules.
func grokContextText(work string) string {
	return "<user_info>\nOS Version: macos\nShell: /bin/sh\nWorkspace Path: " + work + "\nToday's date: 2026-09-27\n</user_info>\n\n" +
		grokRulesOpen +
		"<user_rule>\nState points directly.\n</user_rule>\n\n" +
		"<user_rule>\nVerify UI work in a browser.\n</user_rule>\n" +
		grokRulesClose
}

const judgeWork = "/private/state/model-runs/run/work"

func judgeExpectation() grokExpectation {
	schema, _ := actionSchema(Tools())
	return grokExpectation{model: grokProbeModel, effort: "high", schema: schema, prompt: `{"messages":[]}`, work: judgeWork}
}

// grokRequest is a Responses API body shaped as Grok 1.0.41 sent it.
func grokRequest(want grokExpectation) map[string]any {
	message := func(role, content string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": content}
	}
	return map[string]any{
		"model":            want.model,
		"include":          []string{"reasoning.encrypted_content"},
		"prompt_cache_key": "k",
		"reasoning":        map[string]any{"effort": want.effort, "summary": "concise"},
		"store":            false,
		"stream":           true,
		"text":             map[string]any{"format": map[string]any{"type": "json_schema", "name": "structured_output", "schema": json.RawMessage(want.schema), "strict": true}},
		"input":            []any{message("system", codexInstructions), message("user", grokContextText(want.work)), message("user", want.prompt)},
	}
}

const grokTitleBody = `{"model":"grok-4.6","tools":[{"type":"function","name":"session_title","parameters":{}}],"tool_choice":{"type":"function","name":"session_title"},"input":[{"type":"message","role":"user","content":"<user_query>x</user_query>"}]}`

func encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGrokProbeJudgement(t *testing.T) {
	want := judgeExpectation()
	input := func(r map[string]any) []any { return r["input"].([]any) }
	for _, tc := range []struct {
		name   string
		edit   func(map[string]any)
		title  bool
		code   string
		extras int
	}{
		{name: "pinned shape with the title request", title: true},
		{name: "pinned shape without the title request"},
		{name: "empty tools array", edit: func(r map[string]any) { r["tools"] = []any{} }},
		{name: "native tools", edit: func(r map[string]any) {
			r["tools"] = []any{map[string]any{"type": "function", "name": "run_terminal_cmd"}}
		}, code: "probe_unexpected_tools"},
		{name: "server-side search", edit: func(r map[string]any) { r["tools"] = []any{map[string]any{"type": "web_search"}} }, code: "probe_unexpected_tools"},
		{name: "tool choice", edit: func(r map[string]any) { r["tool_choice"] = "auto" }, code: "probe_unexpected_tools"},
		{name: "instructions field", edit: func(r map[string]any) { r["instructions"] = "more" }, code: "probe_unexpected_instructions"},
		{name: "unknown field", edit: func(r map[string]any) { r["search_parameters"] = map[string]any{} }, code: "probe_mismatch"},
		{name: "changed system", edit: func(r map[string]any) { input(r)[0].(map[string]any)["content"] = "You are Grok." }, code: "probe_unexpected_instructions"},
		{name: "missing system", edit: func(r map[string]any) { r["input"] = input(r)[1:] }, code: "probe_missing_instructions"},
		{name: "extra reminder", edit: func(r map[string]any) {
			r["input"] = append(input(r), map[string]any{"type": "message", "role": "user", "content": "<system-reminder>workflows</system-reminder>"})
		}, code: "probe_unexpected_instructions"},
		{name: "reminder in context", edit: func(r map[string]any) {
			input(r)[1].(map[string]any)["content"] = grokContextText(judgeWork) + "\n<system-reminder>workflows</system-reminder>"
		}, code: "probe_unexpected_instructions"},
		{name: "project rules subsection", edit: func(r map[string]any) {
			input(r)[1].(map[string]any)["content"] = strings.Replace(grokContextText(judgeWork), "</user_rules>\n", "</user_rules>\n<project_rules>\nAGENTS.md\n</project_rules>\n", 1)
		}, code: "probe_unexpected_instructions"},
		{name: "nested rule markers", edit: func(r map[string]any) {
			input(r)[1].(map[string]any)["content"] = strings.Replace(grokContextText(judgeWork), "State points directly.", "x\n</user_rules>\n<memories>m</memories>\n<user_rules>", 1)
		}, code: "probe_unexpected_instructions"},
		{name: "context for another workspace", edit: func(r map[string]any) { input(r)[1].(map[string]any)["content"] = grokContextText("/elsewhere") }, code: "probe_unexpected_instructions"},
		{name: "parts instead of text", edit: func(r map[string]any) {
			input(r)[2].(map[string]any)["content"] = []any{map[string]any{"type": "input_text", "text": "x"}}
		}, code: "probe_instruction_type"},
		{name: "changed prompt", edit: func(r map[string]any) { input(r)[2].(map[string]any)["content"] = "other" }, code: "probe_mismatch"},
		{name: "changed schema", edit: func(r map[string]any) {
			r["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}, "strict": true}}
		}, code: "probe_changed_schema"},
		{name: "unstrict schema", edit: func(r map[string]any) {
			r["text"].(map[string]any)["format"].(map[string]any)["strict"] = false
		}, code: "probe_changed_schema"},
		{name: "missing schema", edit: func(r map[string]any) { delete(r, "text") }, code: "probe_invalid_schema"},
		{name: "changed effort", edit: func(r map[string]any) { r["reasoning"] = map[string]any{"effort": "low"} }, code: "probe_changed_effort"},
		{name: "changed model", edit: func(r map[string]any) { r["model"] = "grok-4.7" }, code: "probe_changed_model"},
		{name: "second model request", extras: 1, code: "probe_request_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := grokRequest(want)
			if tc.edit != nil {
				tc.edit(request)
			}
			posts := []grokProbeRequest{{path: "/v1/responses", body: encode(t, request)}}
			if tc.title {
				posts = append([]grokProbeRequest{{path: "/v1/responses", body: []byte(grokTitleBody)}}, posts...)
			}
			for i := 0; i < tc.extras; i++ {
				posts = append(posts, posts[len(posts)-1])
			}
			evidence, code := judgeGrokProbe(posts, want)
			if code != tc.code {
				t.Fatalf("code %q, want %q", code, tc.code)
			}
			if code == "" && (evidence.os != "macos" || evidence.shell != "/bin/sh" || !strings.HasPrefix(evidence.rules, grokRulesOpen)) {
				t.Fatalf("evidence %+v", evidence)
			}
			if code != "" && code != "probe_request_limit" && !capabilityCode(code) {
				t.Fatalf("%s is not a capability refusal", code)
			}
		})
	}
}

func TestGrokProbeNeedsExactlyOneModelRequest(t *testing.T) {
	want := judgeExpectation()
	if _, code := judgeGrokProbe([]grokProbeRequest{{path: "/v1/responses", body: []byte(grokTitleBody)}}, want); code != "probe_no_requests" {
		t.Fatalf("a probe with only a title request: %q", code)
	}
	body := encode(t, grokRequest(want))
	if _, code := judgeGrokProbe([]grokProbeRequest{{path: "/v1/chat/completions", body: body}}, want); code != "probe_mismatch" {
		t.Fatalf("a request on another API: %q", code)
	}
	titles := []grokProbeRequest{{path: "/v1/responses", body: []byte(grokTitleBody)}, {path: "/v1/responses", body: []byte(grokTitleBody)}, {path: "/v1/responses", body: []byte(grokTitleBody)}, {path: "/v1/responses", body: body}}
	if _, code := judgeGrokProbe(titles, want); code != "probe_request_limit" {
		t.Fatalf("unbounded side requests: %q", code)
	}
}

// A request that only resembles a title request is judged as a model request.
func TestGrokTitleRequestIsNarrow(t *testing.T) {
	if !grokTitleRequest([]byte(grokTitleBody)) {
		t.Fatal("the title request was not recognized")
	}
	for _, body := range []string{
		`{"tools":[{"type":"function","name":"session_title"},{"type":"function","name":"shell"}],"tool_choice":{"type":"function","name":"session_title"}}`,
		`{"tools":[{"type":"function","name":"session_title"}],"tool_choice":"auto"}`,
		`{"tools":[{"type":"function","name":"session_title"}],"tool_choice":{"type":"function","name":"session_title"},"text":{"format":{"type":"json_schema"}}}`,
	} {
		if grokTitleRequest([]byte(body)) {
			t.Fatalf("mistaken for a title request: %s", body)
		}
	}
}

func TestGrokContextAcceptsOnlyThePinnedShape(t *testing.T) {
	if _, ok := parseGrokContext(grokContextText(judgeWork), judgeWork); !ok {
		t.Fatal("the pinned context was refused")
	}
	noRules := strings.Replace(grokContextText(judgeWork), "<user_rule>\nState points directly.\n</user_rule>\n\n<user_rule>\nVerify UI work in a browser.\n</user_rule>\n", "", 1)
	if _, ok := parseGrokContext(noRules, judgeWork); !ok {
		t.Fatal("an empty built-in rule list was refused")
	}
	for name, text := range map[string]string{
		"extra info line": strings.Replace(grokContextText(judgeWork), "Shell: /bin/sh\n", "Shell: /bin/sh\nMemory: on\n", 1),
		"bad date":        strings.Replace(grokContextText(judgeWork), "2026-09-27", "today", 1),
		"text after":      grokContextText(judgeWork) + "\nmore",
		"rule unclosed":   strings.Replace(grokContextText(judgeWork), "\n</user_rule>\n\n<user_rule>", "\n\n<user_rule>", 1),
	} {
		if _, ok := parseGrokContext(text, judgeWork); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func grokTranscript(t *testing.T, work, prompt string, extra ...map[string]any) []byte {
	t.Helper()
	lines := []any{
		map[string]any{"type": "system", "content": codexInstructions},
		map[string]any{"type": "user", "content": []any{map[string]any{"type": "text", "text": grokContextText(work)}}},
		map[string]any{"type": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}, "prompt_index": 0},
	}
	for _, e := range extra {
		lines = append(lines, e)
	}
	var out []byte
	for _, line := range lines {
		out = append(append(out, encode(t, line)...), '\n')
	}
	return out
}

func TestGrokTranscriptJudgement(t *testing.T) {
	evidence, ok := parseGrokContext(grokContextText(judgeWork), judgeWork)
	if !ok {
		t.Fatal("context refused")
	}
	const work = "/private/state/model-runs/other/work"
	assistant := map[string]any{"type": "assistant", "content": "{}"}
	if code := evidence.judgeTranscript(grokTranscript(t, work, "p", assistant), "p", work); code != "" {
		t.Fatalf("a matching transcript was refused: %s", code)
	}
	changed := evidence
	changed.rules = strings.Replace(changed.rules, "State points directly.", "Different.", 1)
	for name, tc := range map[string]struct {
		evidence   grokEvidence
		transcript []byte
		code       string
	}{
		"missing":                 {evidence, nil, "missing_native_transcript"},
		"rules differ from probe": {changed, grokTranscript(t, work, "p"), "unexpected_native_instructions"},
		"reminder after prompt":   {evidence, grokTranscript(t, work, "p", map[string]any{"type": "user", "content": "nudge"}), "unexpected_native_instructions"},
		"system after prompt":     {evidence, grokTranscript(t, work, "p", map[string]any{"type": "system", "content": "more"}), "unexpected_native_instructions"},
		"different prompt":        {evidence, grokTranscript(t, work, "other"), "unexpected_native_instructions"},
		"truncated":               {evidence, grokTranscript(t, work, "p")[:40], "unexpected_native_instructions"},
	} {
		if code := tc.evidence.judgeTranscript(tc.transcript, "p", work); code != tc.code {
			t.Errorf("%s: %q want %q", name, code, tc.code)
		}
	}
}

func streamOf(lines ...string) *grokStream {
	stream := newGrokStream(Tools())
	for _, line := range lines {
		if stream.observe([]byte(line)) != "" {
			break
		}
	}
	return stream
}

const (
	grokCatalog = `{"type":"available_commands","tools":[],"commands":["compact"]}`
	grokEnd     = `{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":1,"output_tokens":6,"reasoning_tokens":2},"total_cost_usd":0.5,"total_cost_usd_ticks":123000000,"structuredOutput":{"content":"done","tool_calls":[{"name":"read_state","arguments":"{\"k\":1}"}]}}`
)

func TestGrokStreamSuccessAndAccounting(t *testing.T) {
	result, failure := streamOf(grokCatalog, `{"type":"thought","data":"x"}`, `{"type":"text","data":"{}"}`, `{"type":"usage","usage":{"input_tokens":1,"output_tokens":1}}`, grokEnd).result()
	if failure != nil {
		t.Fatal(failure)
	}
	want := harness.Usage{Known: true, Input: 15, Output: 6, CacheRead: 4, CacheWrite: 1, Reasoning: 2, CacheKnown: true, ReasoningKnown: true}
	if result.Usage != want {
		t.Fatalf("usage %+v", result.Usage)
	}
	if result.Cost != (harness.Cost{USD: 0.0123, Known: true}) {
		t.Fatalf("ticks must win over the float valuation: %+v", result.Cost)
	}
	calls := result.Message.ToolCalls
	if result.Message.Content != "done" || len(calls) != 1 || calls[0].Function.Name != "read_state" || !strings.HasPrefix(calls[0].ID, "call_") {
		t.Fatalf("message %+v", result.Message)
	}
}

func TestGrokStreamFailures(t *testing.T) {
	end := func(fields string) string {
		return `{"type":"end","usage":{"input_tokens":1,"output_tokens":1},"total_cost_usd_ticks":10` + fields + `}`
	}
	rateLimited := `{"type":"error","message":"Internal error: {\n  \"message\": \"API error (status 429 Too Many Requests): slow down\",\n  \"http_status\": 429\n}"}`
	for _, tc := range []struct {
		name      string
		lines     []string
		code      string
		cause     harness.Cause
		retryable bool
		known     bool
	}{
		{"native tool catalog", []string{`{"type":"available_commands","tools":["read_file"]}`, grokEnd}, "unexpected_native_tool_catalog", harness.CauseUnknown, false, false},
		{"native tool call", []string{grokCatalog, `{"type":"tool_call","toolCallId":"c","toolName":"run_terminal_cmd"}`, grokEnd}, "unexpected_native_tool_call", harness.CauseUnknown, false, false},
		{"unknown tool event", []string{grokCatalog, `{"type":"server_tool_use"}`, grokEnd}, "unexpected_native_tool_call", harness.CauseUnknown, false, false},
		{"no catalog", []string{grokEnd}, "missing_native_tool_catalog", harness.CauseUnknown, false, true},
		{"malformed", []string{grokCatalog, `{`, grokEnd}, "malformed_event_json", harness.CauseUnknown, false, true},
		{"missing end", []string{grokCatalog, `{"type":"text","data":"x"}`}, "missing_terminal_result", harness.CauseUnknown, false, false},
		{"two ends", []string{grokCatalog, grokEnd, grokEnd}, "duplicate_terminal_result", harness.CauseUnknown, false, false},
		{"max tokens", []string{grokCatalog, end(`,"stopReason":"max_tokens"`)}, "max_tokens", harness.CauseUnknown, false, true},
		{"refusal", []string{grokCatalog, end(`,"stopReason":"refusal"`)}, "refusal", harness.CauseUnknown, false, true},
		{"unknown stop", []string{grokCatalog, end(`,"stopReason":"paused"`)}, "unexpected_stop_reason", harness.CauseUnknown, false, true},
		{"failed end", []string{grokCatalog, end(`,"status":"failed","stopReason":"end_turn"`)}, "end_error", harness.CauseUnknown, false, true},
		{"no structured output", []string{grokCatalog, end(`,"stopReason":"end_turn"`)}, "missing_structured_output", harness.CauseUnknown, false, true},
		{"invalid envelope", []string{grokCatalog, end(`,"stopReason":"end_turn","structuredOutput":{"content":"x","tool_calls":[{"name":"shell","arguments":"{}"}]}`)}, "invalid_action_envelope", harness.CauseUnknown, false, true},
		{"clean rate limit", []string{grokCatalog, rateLimited}, "http_429", harness.CauseRateLimited, true, false},
		{"rate limit after output", []string{grokCatalog, `{"type":"text","data":"x"}`, rateLimited}, "http_429", harness.CauseUnknown, false, false},
		{"unclassified error", []string{grokCatalog, `{"type":"error","message":"secret provider prose"}`, end(`,"stopReason":"error"`)}, "grok_error", harness.CauseUnknown, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, failure := streamOf(tc.lines...).result()
			if failure == nil {
				t.Fatal("accepted")
			}
			if failure.Code != tc.code || failure.Cause != tc.cause || failure.Retryable() != tc.retryable || failure.Phase != PhaseResponse {
				t.Fatalf("%+v", failure)
			}
			if facts := failure.HarnessFacts(); facts.Family != harness.FailureRequest {
				t.Fatalf("a failure after launch is a request failure: %+v", facts)
			}
			if result.Message.Content != "" || result.Message.ToolCalls != nil {
				t.Fatal("a failure carried a proposal")
			}
			if result.Usage.Known != tc.known {
				t.Fatalf("usage known %v", result.Usage.Known)
			}
			if strings.Contains(failure.Error(), "secret") || strings.Contains(failure.Error(), "slow down") {
				t.Fatal("provider text reached the error")
			}
		})
	}
}

func TestGrokStreamSpendRules(t *testing.T) {
	for _, tc := range []struct {
		name              string
		lines             []string
		usageKnown, costK bool
		usd               float64
	}{
		{"incomplete usage", []string{grokCatalog, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"usage_is_incomplete":true,"total_cost_usd_ticks":10}`}, false, false, 0},
		{"partial cost", []string{grokCatalog, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"cost_is_partial":true,"total_cost_usd_ticks":10}`}, true, false, 0},
		{"float cost only", []string{grokCatalog, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"total_cost_usd":0.25}`}, true, true, 0.25},
		{"no spend", []string{grokCatalog, `{"type":"end","stopReason":"end_turn"}`}, false, false, 0},
		{"error spend when end has none", []string{grokCatalog, `{"type":"error","message":"x","usage":{"input_tokens":7,"output_tokens":1},"total_cost_usd_ticks":5}`, `{"type":"end","stopReason":"error"}`}, true, true, 5e-10},
		{"negative count", []string{grokCatalog, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":-1,"output_tokens":1}}`}, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := streamOf(tc.lines...).result()
			if result.Usage.Known != tc.usageKnown || result.Cost.Known != tc.costK || result.Cost.USD != tc.usd {
				t.Fatalf("%+v %+v", result.Usage, result.Cost)
			}
		})
	}
}

// Observation stops at the first unexpected surface: later lines are never
// read, so a proposal after a native tool call cannot be accepted.
func TestGrokStreamStopsAtTheFirstNativeSurface(t *testing.T) {
	stream := newGrokStream(Tools())
	if stream.observe([]byte(grokCatalog)) != "" {
		t.Fatal("an empty catalog stopped the stream")
	}
	if stream.observe([]byte(`{"type":"tool_call_update","toolCallId":"c","status":"completed"}`)) != "unexpected_native_tool_call" {
		t.Fatal("a native tool update did not stop the stream")
	}
	stream.observe([]byte(grokEnd))
	if stream.ends != 0 {
		t.Fatal("a line after the stop was read")
	}
}

// Lines are judged as they arrive, across writes, and the first unexpected
// surface stops the process.
func TestGrokLinesStopAtTheFirstNativeSurface(t *testing.T) {
	stream := newGrokStream(Tools())
	stopped := 0
	lines := &grokLines{stop: func() { stopped++ }, observe: stream.observe}
	for _, chunk := range []string{grokCatalog[:10], grokCatalog[10:] + "\n{\"type\":\"te", "xt\",\"data\":\"x\"}\n"} {
		if _, err := lines.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if !stream.catalog || !stream.activity {
		t.Fatal("a line split across writes was not judged")
	}
	if _, err := lines.Write([]byte(`{"type":"tool_call"}` + "\n" + grokEnd + "\n")); err == nil || stopped != 1 || !lines.stopped {
		t.Fatalf("a native tool call did not stop the process: %v %d", err, stopped)
	}
	if stream.ends != 0 {
		t.Fatal("a line after the stop was judged")
	}
}
