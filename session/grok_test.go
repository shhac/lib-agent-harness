package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// A synthetic ACP agent: this test binary re-executed as `grok agent ... stdio`.
// It answers with the shapes grok 1.0.41 was observed to use, performs no
// inference and reads no login. It logs what it saw to a file the test reads.
const (
	grokFixtureEnv   = "LIB_HARNESS_GROK_FIXTURE"
	grokFixtureLog   = "LIB_HARNESS_GROK_FIXTURE_LOG"
	grokFixtureModel = "LIB_HARNESS_GROK_FIXTURE_MODEL"
)

func init() {
	if os.Getenv(grokFixtureEnv) != "1" {
		return
	}
	os.Exit(runGrokFixture())
}

func runGrokFixture() int {
	logFile, err := os.OpenFile(os.Getenv(grokFixtureLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 2
	}
	defer logFile.Close()
	record := func(kind, value string) { _, _ = logFile.WriteString(kind + "\t" + value + "\n") }
	record("args", strings.Join(os.Args[1:], " "))
	record("home", os.Getenv("GROK_HOME"))
	record("xai", os.Getenv("XAI_API_KEY"))
	record("telemetry", os.Getenv("GROK_TELEMETRY_ENABLED"))
	model, effort := "grok-default", ""
	for _, arg := range os.Args[1:] {
		if value, ok := strings.CutPrefix(arg, "--model="); ok {
			model = value
		}
		if value, ok := strings.CutPrefix(arg, "--reasoning-effort="); ok {
			effort = value
		}
	}
	if override := os.Getenv(grokFixtureModel); override != "" {
		model = override
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 4<<20)
	out := json.NewEncoder(os.Stdout)
	send := func(m map[string]any) bool { return out.Encode(m) == nil }
	next := func() (map[string]json.RawMessage, bool) {
		if !in.Scan() {
			return nil, false
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(in.Bytes(), &m) != nil || str(m, "jsonrpc") != "2.0" {
			return nil, false
		}
		return m, true
	}
	update := func(session string, u map[string]any) bool {
		return send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": u}})
	}
	responseCompleted := func(session string, usage map[string]any) bool {
		return send(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/session_notification", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "response_completed", "usage": usage}}})
	}
	sessionState := func() map[string]any {
		state := map[string]any{"models": map[string]any{"currentModelId": model, "availableModels": []any{map[string]any{"modelId": model, "_meta": map[string]any{"totalContextTokens": 1000}}}}}
		if effort != "" {
			state["configOptions"] = []any{map[string]any{"id": "model", "currentValue": model}, map[string]any{"id": "reasoning_effort", "currentValue": effort}}
		}
		return state
	}
	for {
		m, ok := next()
		if !ok {
			return 0
		}
		id := m["id"]
		reply := func(result any) bool { return send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		var p map[string]json.RawMessage
		_ = json.Unmarshal(m["params"], &p)
		switch str(m, "method") {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}}})
		case "session/new":
			record("new", string(m["params"]))
			state := sessionState()
			state["sessionId"] = "grok-session-1"
			reply(state)
		case "session/resume":
			record("resume", string(m["params"]))
			if str(p, "sessionId") != "grok-session-1" {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "Path not found.", "data": map[string]any{"detail": "No such file or directory", "code": "FS_NOT_FOUND"}}})
				continue
			}
			reply(sessionState())
		case "_x.ai/session/delete":
			record("delete", str(p, "sessionId"))
			reply(map[string]any{"success": true})
		case "_x.ai/auth/check_subscription":
			reply(map[string]any{"authenticated": true, "meta": map[string]any{"email": "fixture@example.test", "auth_mode": "oauth", "subscription_tier": "Fixture", "team_name": nil}})
		case "session/prompt":
			var prompt struct {
				Session string `json:"sessionId"`
				Prompt  []struct{ Type, Text string }
			}
			if json.Unmarshal(m["params"], &prompt) != nil || len(prompt.Prompt) != 1 || prompt.Prompt[0].Type != "text" {
				return 3
			}
			session, text := prompt.Session, prompt.Prompt[0].Text
			record("prompt", text)
			switch {
			case strings.HasSuffix(text, "permission"):
				update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "title": "run_terminal_command", "rawInput": map[string]any{"command": "touch x"}, "_meta": map[string]any{"x.ai/tool": map[string]any{"name": "run_terminal_command"}}})
				send(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/request_permission", "params": map[string]any{"sessionId": session, "toolCall": map[string]any{"toolCallId": "call_1"}, "options": []any{
					map[string]any{"optionId": "always-allow", "kind": "allow_always"}, map[string]any{"optionId": "allow-once", "kind": "allow_once"},
					map[string]any{"optionId": "reject-once", "kind": "reject_once"}, map[string]any{"optionId": "reject-always", "kind": "reject_always"}}}})
				answer, ok := next()
				if !ok || string(answer["id"]) != "0" {
					return 3
				}
				var outcome struct {
					Outcome struct{ Outcome, OptionID string } `json:"outcome"`
				}
				_ = json.Unmarshal(answer["result"], &outcome)
				record("permission", outcome.Outcome.Outcome+":"+outcome.Outcome.OptionID)
				if outcome.Outcome.OptionID != "allow-once" {
					update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "failed"})
					reply(map[string]any{"stopReason": "cancelled", "_meta": map[string]any{"cancellationCategory": "PermissionRejected", "usage": map[string]any{"inputTokens": 10, "outputTokens": 2, "cachedReadTokens": 0, "cacheCreationTokens": 0}}})
					continue
				}
				update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed"})
				reply(map[string]any{"stopReason": "end_turn", "_meta": map[string]any{}})
			case strings.HasSuffix(text, "wait"):
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "working"}})
				cancel, ok := next()
				if !ok || str(cancel, "method") != "session/cancel" || len(cancel["id"]) != 0 {
					return 3
				}
				record("cancel", "")
				reply(map[string]any{"stopReason": "cancelled", "_meta": map[string]any{"cancellationCategory": "MidTurnAbort"}})
			case strings.HasSuffix(text, "fail"):
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "Internal error", "data": map[string]any{"message": "secret provider text", "http_status": 429}}})
			default:
				update(session, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "thinking"}})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Let me look. "}})
				update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "title": "Read `/secret/path`", "_meta": map[string]any{"x.ai/tool": map[string]any{"name": "read_file"}}})
				responseCompleted(session, map[string]any{"input_tokens": 10, "output_tokens": 4, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 0, "reasoning_tokens": 1})
				update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "in_progress"})
				update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed"})
				update("another-session", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "not ours"}})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "fixture "}})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "answer"}})
				responseCompleted(session, map[string]any{"input_tokens": 5, "output_tokens": 2, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 0, "reasoning_tokens": 0})
				// An agent request the client never offered must be refused, not ignored.
				send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "fs/read_text_file", "params": map[string]any{"sessionId": session, "path": "/etc/passwd"}})
				answer, ok := next()
				if !ok || string(answer["id"]) != "7" || len(answer["error"]) == 0 {
					return 3
				}
				record("refused", "fs/read_text_file")
				reply(map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"usage": map[string]any{"inputTokens": 85, "outputTokens": 6, "totalTokens": 91, "cachedReadTokens": 70, "cacheCreationTokens": 0, "reasoningTokens": 1, "modelCalls": 2}}})
			}
		default:
			if len(id) != 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "Method not found"}})
			}
		}
	}
}

// grokFixtureLogged reads the fixture's log as kind -> values.
func grokFixtureLogged(t *testing.T, log string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		kind, value, _ := strings.Cut(line, "\t")
		out[kind] = append(out[kind], value)
	}
	return out
}

func grokFixtureOptions(t *testing.T) (Options, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fixture.log")
	t.Setenv(grokFixtureEnv, "1")
	t.Setenv(grokFixtureLog, log)
	return Options{Provider: harness.Provider{Engine: harness.Grok, CLI: harness.CLI{Binary: os.Args[0], Home: t.TempDir()}}, WorkDir: t.TempDir(), Policy: Policy{GrokPermission: GrokDenyWhenAsked}}, log
}

func drain(t *testing.T, turn *Turn) []Event {
	t.Helper()
	var events []Event
	for e := range turn.Events() {
		events = append(events, e)
	}
	return events
}

func TestGrokSessionOverTheAgentProtocol(t *testing.T) {
	o, log := grokFixtureOptions(t)
	t.Setenv("XAI_API_KEY", "ambient-secret")
	o.Model, o.Effort = "grok-test", "low"
	o.Instructions = Instructions{Mode: Append, Text: "be brief"}
	o.Policy.GrokTelemetry = GrokTelemetryReduced
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	if ref.Engine != harness.Grok || ref.ID != "grok-session-1" || ref.Home != o.Provider.CLI.Home {
		t.Fatalf("reference: %+v", ref)
	}
	if caps := s.Capabilities(); caps.Start.Availability != harness.Native || caps.AppendInstructions.Availability != harness.Native {
		t.Fatalf("capabilities: %+v", caps)
	}
	turn, err := s.StartTurn(ctx, Input{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(t, turn)
	result, err := turn.Wait(ctx)
	if err != nil || result.Status != "completed" || result.Text != "fixture answer" {
		t.Fatalf("%+v %v", result, err)
	}
	var kinds []string
	var observed []Usage
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		if e.Kind == "tool_started" && (e.Tool != "read_file" || e.ItemID != "call_1") {
			t.Errorf("tool named from its title or misidentified: %+v", e)
		}
		if e.Kind == "text_delta" && strings.Contains(e.Text, "not ours") {
			t.Error("another session's update reached this turn")
		}
		if e.Kind == "usage" {
			observed = append(observed, *e.Usage)
		}
	}
	if slices.Index(kinds, "tool_started") < 0 || slices.Index(kinds, "tool_completed") < 0 || kinds[len(kinds)-1] != "status" {
		t.Fatalf("events: %v", kinds)
	}
	if len(observed) != 3 || observed[0].Final || observed[0].Input != 40 || observed[1].Input != 85 || observed[1].Final || !observed[2].Final {
		t.Fatalf("usage observations: %+v", observed)
	}
	final := result.Usage
	if !final.Final || final.Input != 85 || final.CacheRead != 70 || final.Output != 6 || final.Reasoning != 1 || !final.CacheKnown {
		t.Fatalf("turn accounting: %+v", final)
	}
	if fresh, ok := final.Fresh(); !ok || fresh != 15 {
		t.Fatalf("fresh input %d %v", fresh, ok)
	}
	if result.Observed.Input != 85 || result.Observed.Final {
		t.Fatalf("observed: %+v", result.Observed)
	}
	c := result.Context
	if c.Quality != harness.Estimated || c.UsedTokens == nil || *c.UsedTokens != 45 || c.CapacityTokens == nil || *c.CapacityTokens != 1000 || c.Model != "grok-test" {
		t.Fatalf("context: %+v", c)
	}
	account, err := s.ReadAccount(ctx)
	if err != nil || account.Plan != "Fixture" || account.LoggedIn == nil || !*account.LoggedIn {
		t.Fatalf("account %+v %v", account, err)
	}
	if _, err = s.ReadQuota(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("quota: %v", err)
	}
	if _, err = s.Compact(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("compact: %v", err)
	}
	s.Close()

	logged := grokFixtureLogged(t, log)
	if logged["args"][0] != "agent --no-leader --model=grok-test --reasoning-effort=low stdio" {
		t.Errorf("arguments: %q", logged["args"])
	}
	if logged["home"][0] != o.Provider.CLI.Home || logged["xai"][0] != "" || logged["telemetry"][0] != "0" {
		t.Errorf("environment: home %q xai %q telemetry %q", logged["home"], logged["xai"], logged["telemetry"])
	}
	var created struct {
		Cwd  string         `json:"cwd"`
		MCP  []any          `json:"mcpServers"`
		Meta map[string]any `json:"_meta"`
	}
	if json.Unmarshal([]byte(logged["new"][0]), &created) != nil || created.Cwd != o.WorkDir || created.MCP == nil || len(created.MCP) != 0 || created.Meta["rules"] != "be brief" {
		t.Errorf("session/new: %s", logged["new"])
	}
	if len(logged["refused"]) != 1 {
		t.Error("an unoffered agent request was not refused")
	}

	resumed, err := Resume(ctx, o, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.Ref() != ref {
		t.Fatal("resume changed the reference")
	}
	var resumedWith struct {
		Session string         `json:"sessionId"`
		Meta    map[string]any `json:"_meta"`
	}
	logged = grokFixtureLogged(t, log)
	// Grok forgets the session's instructions on a resume that omits them.
	if json.Unmarshal([]byte(logged["resume"][0]), &resumedWith) != nil || resumedWith.Session != "grok-session-1" || resumedWith.Meta["rules"] != "be brief" {
		t.Errorf("session/resume: %s", logged["resume"])
	}
}

func TestGrokOpenStartsFreshWhenTheConversationIsGone(t *testing.T) {
	o, _ := grokFixtureOptions(t)
	ctx := testContext(t)
	n, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	gone := reference(n, "grok-session-gone")
	s, opened, err := Open(ctx, o, &gone)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if opened.Fresh != FreshUnavailable || s.Ref().ID != "grok-session-1" {
		t.Fatalf("%+v %+v", opened, s.Ref())
	}
}

func TestGrokPermissionRequestsFollowThePolicy(t *testing.T) {
	for _, tc := range []struct {
		policy, answer, status, code string
	}{
		{GrokDenyWhenAsked, "selected:reject-once", "failed", grokPermissionRejected},
		{GrokAllowWhenAsked, "selected:allow-once", "completed", ""},
	} {
		t.Run("policy "+tc.policy, func(t *testing.T) {
			o, log := grokFixtureOptions(t)
			o.Policy.GrokPermission = tc.policy
			ctx := testContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			turn, err := s.StartTurn(ctx, Input{Text: "permission"})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, turn)
			result, err := turn.Wait(ctx)
			var failure *TurnError
			if result.Status != tc.status || (tc.code != "" && (!errors.As(err, &failure) || failure.Code != tc.code)) || (tc.code == "" && err != nil) {
				t.Fatalf("%+v %v", result, err)
			}
			if got := grokFixtureLogged(t, log)["permission"]; len(got) != 1 || got[0] != tc.answer {
				t.Fatalf("permission answered %q", got)
			}
		})
	}
}

func TestGrokPermissionNeverGrantsMoreThanOnce(t *testing.T) {
	always := json.RawMessage(`{"options":[{"optionId":"always-allow","kind":"allow_always"},{"optionId":"reject-always","kind":"reject_always"}]}`)
	for _, policy := range []string{GrokDenyWhenAsked, GrokAllowWhenAsked} {
		if got := grokPermissionOutcome(always, policy); got["outcome"] != "cancelled" {
			t.Errorf("%s chose a persistent option: %v", policy, got)
		}
	}
	if got := grokPermissionOutcome(json.RawMessage(`not json`), GrokAllowWhenAsked); got["outcome"] != "cancelled" {
		t.Errorf("an unreadable request was allowed: %v", got)
	}
	reply := grokServerReply(map[string]json.RawMessage{"id": json.RawMessage(`3`), "method": json.RawMessage(`"terminal/create"`)}, GrokAllowWhenAsked)
	if reply["error"] == nil || reply["result"] != nil || reply["jsonrpc"] != "2.0" {
		t.Errorf("an unhandled request was not refused: %v", reply)
	}
}

func TestGrokInterruptAndComposedSteer(t *testing.T) {
	o, log := grokFixtureOptions(t)
	ctx := testContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	turn, err := s.StartTurn(ctx, Input{Text: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	go drain(t, turn)
	if _, err = s.Steer(ctx, turn.ID(), Input{Text: "x"}, SteerOptions{RequireNative: true}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("native steering was claimed: %v", err)
	}
	steered, err := s.Steer(ctx, turn.ID(), Input{Text: "now wait"}, SteerOptions{})
	if err != nil || steered.Strategy != harness.Composed || steered.Turn == turn {
		t.Fatalf("%+v %v", steered, err)
	}
	if result, _ := turn.Wait(ctx); result.Status != "interrupted" || result.NativeError {
		t.Fatalf("steered turn: %+v", result)
	}
	go drain(t, steered.Turn)
	if err = s.Interrupt(ctx, steered.Turn.ID()); err != nil {
		t.Fatal(err)
	}
	if caps := s.Capabilities(); caps.Interrupt.Availability != harness.Native || caps.Steer.Availability != harness.Composed {
		t.Fatalf("%+v", caps)
	}
	if got := grokFixtureLogged(t, log)["cancel"]; len(got) != 2 {
		t.Fatalf("cancellations: %d", len(got))
	}
}

func TestGrokRefusedPromptFailsOnlyItsTurn(t *testing.T) {
	o, _ := grokFixtureOptions(t)
	ctx := testContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	turn, err := s.StartTurn(ctx, Input{Text: "fail"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	result, err := turn.Wait(ctx)
	var failure *TurnError
	if !errors.As(err, &failure) || failure.Code != acpRateLimited || result.Usage.Known || !result.NativeError {
		t.Fatalf("%+v %v", result, err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("provider text reached the error")
	}
	turn, err = s.StartTurn(ctx, Input{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	if result, err = turn.Wait(ctx); err != nil || result.Status != "completed" {
		t.Fatalf("the session did not survive a refused prompt: %+v %v", result, err)
	}
}

// Grok substitutes another model for one the login may not use, without
// failing. The session is refused rather than run on a model nobody chose, and
// the empty conversation is removed.
func TestGrokSubstitutedModelIsRefused(t *testing.T) {
	o, log := grokFixtureOptions(t)
	t.Setenv(grokFixtureModel, "grok-other")
	o.Model = "grok-test"
	_, err := Start(testContext(t), o)
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityChangedModel || failure.Phase != BeforeFirstPrompt {
		t.Fatalf("%v", err)
	}
	if got := grokFixtureLogged(t, log)["delete"]; len(got) != 1 || got[0] != "grok-session-1" {
		t.Fatalf("the refused session was not removed: %q", got)
	}
}

func TestGrokEffortMustBeHonoured(t *testing.T) {
	o := Options{Model: "m", Effort: "high"}
	for _, tc := range []struct {
		state grokSessionState
		want  string
	}{
		{grokSessionState{model: "m", effort: "high", effortOffered: true}, ""},
		{grokSessionState{model: "m"}, CapabilityChangedEffort},
		{grokSessionState{model: "m", effort: "low", effortOffered: true}, CapabilityChangedEffort},
		{grokSessionState{model: "n", effort: "high", effortOffered: true}, CapabilityChangedModel},
	} {
		if got := grokConfigMismatch(o, tc.state); got != tc.want {
			t.Errorf("%+v: %q", tc.state, got)
		}
	}
	if grokConfigMismatch(Options{}, grokSessionState{model: "anything"}) != "" {
		t.Error("a session with no requested model was refused")
	}
}

func TestGrokStopReasons(t *testing.T) {
	for _, tc := range []struct {
		stop, category string
		interrupted    bool
		status, code   string
	}{
		{"end_turn", "", false, "completed", ""},
		{"cancelled", "MidTurnAbort", true, "interrupted", ""},
		{"cancelled", "PermissionRejected", false, "failed", grokPermissionRejected},
		{"cancelled", "", false, "failed", grokCancelled},
		{"max_tokens", "", false, "failed", "max_tokens"},
		{"refusal", "", false, "failed", "refusal"},
		{"something_new", "", false, "failed", grokStopUnrecognized},
	} {
		status, code := grokOutcome(tc.stop, tc.category, tc.interrupted)
		if status != tc.status || code != tc.code {
			t.Errorf("%+v: %s %s", tc, status, code)
		}
	}
}

func TestGrokUsageScopes(t *testing.T) {
	// One response: input_tokens excludes the cache, so Input adds it back.
	response := parseGrokResponseUsage(json.RawMessage(`{"input_tokens":179,"output_tokens":117,"cache_read_input_tokens":25344,"cache_creation_input_tokens":0,"reasoning_tokens":28}`))
	if !response.Known || response.Input != 25523 || response.CacheRead != 25344 || !response.CacheKnown || response.Reasoning != 28 {
		t.Fatalf("%+v", response)
	}
	// A turn: inputTokens already counts the cache.
	turn := parseGrokTurnUsage(json.RawMessage(`{"inputTokens":77132,"outputTokens":246,"cachedReadTokens":76544,"cacheCreationTokens":0,"reasoningTokens":47}`))
	if !turn.Known || turn.Input != 77132 || turn.CacheRead != 76544 || !turn.CacheKnown {
		t.Fatalf("%+v", turn)
	}
	if u := parseGrokTurnUsage(json.RawMessage(`{"inputTokens":10,"outputTokens":1}`)); !u.Known || u.CacheKnown {
		t.Fatalf("a missing cache split was claimed: %+v", u)
	}
	for _, raw := range []string{``, `{}`, `{"inputTokens":10}`, `{"inputTokens":10,"outputTokens":1,"cachedReadTokens":11}`, `{"inputTokens":-1,"outputTokens":1}`, `{"inputTokens":10,"outputTokens":1,"reasoningTokens":2}`} {
		if u := parseGrokTurnUsage(json.RawMessage(raw)); u.Known {
			t.Errorf("%s read as known: %+v", raw, u)
		}
	}
	if u := parseGrokResponseUsage(json.RawMessage(`{"input_tokens":9223372036854775807,"output_tokens":1,"cache_read_input_tokens":1}`)); u.Known {
		t.Errorf("an overflowing total read as known: %+v", u)
	}
}

func TestGrokRefusalsAreClassifiedWithoutProviderText(t *testing.T) {
	for _, tc := range []struct {
		reply string
		code  string
		is    error
	}{
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"Method not found"}}`, acpMethodMissing, ErrUnsupported},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32000,"message":"Authentication required","data":"no auth method id provided"}}`, acpAuthRequired, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"Path not found.","data":{"code":"FS_NOT_FOUND"}}}`, grokSessionNotFound, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"Internal error","data":{"message":"secret","http_status":401}}}`, acpAuthFailed, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"Internal error","data":{"message":"secret","http_status":503}}}`, acpProviderUnavailable, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"secret"}}`, acpRejected, ErrRejected},
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.reply), &m); err != nil {
			t.Fatal(err)
		}
		id, r, isReply, err := parseGrokReply(m)
		var refusal *acpRefusal
		if id != "1" || !isReply || err != nil || !errors.As(r.err, &refusal) || refusal.code != tc.code || !errors.Is(r.err, tc.is) {
			t.Fatalf("%s: %q %+v %v %v", tc.reply, id, r, isReply, err)
		}
		if strings.Contains(r.err.Error(), "secret") {
			t.Fatal("provider text reached the error")
		}
		if facts, ok := harness.ErrorFacts(r.err); !ok || facts.Code != tc.code || facts.Engine != harness.Grok {
			t.Fatalf("facts: %+v %v", facts, ok)
		}
	}
}

func TestGrokEnvironmentAndReference(t *testing.T) {
	t.Setenv("GROK_HOME", "/ambient/grok")
	t.Setenv("XAI_API_KEY", "ambient-secret")
	t.Setenv("GROK_AUTH_PROVIDER_ACCESS_TOKEN", "ambient-token")
	t.Setenv("GROK_CLI_CHAT_PROXY_BASE_URL", "https://elsewhere.example")
	t.Setenv("GROK_MEMORY", "1")
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Grok}, WorkDir: t.TempDir(), Policy: Policy{GrokPermission: GrokDenyWhenAsked}})
	if err != nil {
		t.Fatal(err)
	}
	ambient, _ := filepath.Abs("/ambient/grok")
	if o.Provider.CLI.Home != ambient || o.Provider.CLI.Binary != "grok" || o.Policy.GrokPermission != GrokDenyWhenAsked {
		t.Fatalf("defaults: %+v", o)
	}
	env := environment(o)
	if envValue(env, "GROK_HOME") != ambient || envValue(env, "XAI_API_KEY") != "" || envValue(env, "GROK_AUTH_PROVIDER_ACCESS_TOKEN") != "" || envValue(env, "GROK_CLI_CHAT_PROXY_BASE_URL") != "" {
		t.Fatalf("environment kept a credential or override: %v", env)
	}
	if envValue(env, "GROK_MEMORY") != "1" {
		t.Fatal("the zero telemetry policy changed the operator's own setting")
	}
	o.Policy.GrokTelemetry = GrokTelemetryReduced
	reduced := environment(o)
	if envValue(reduced, "GROK_MEMORY") != "0" || envValue(reduced, "GROK_TELEMETRY_ENABLED") != "0" {
		t.Fatal("the reduced telemetry policy was not applied")
	}
	if reference(o, "s1") == reference(Options{Provider: o.Provider, WorkDir: o.WorkDir, Policy: Policy{GrokPermission: GrokDenyWhenAsked}}, "s1") {
		t.Fatal("the telemetry policy is not part of the reference")
	}
	codex := o
	codex.Provider.Engine = harness.Codex
	if compatible(codex, reference(o, "s1")) {
		t.Fatal("a Grok reference resumed as Codex")
	}
}

// Grok references are persisted like any other; this pins their form.
// A Grok session has no permission default: its agent mode runs edits and
// commands without asking, so the caller must choose knowingly.
func TestGrokPermissionPolicyIsRequired(t *testing.T) {
	_, err := normalize(Options{Provider: harness.Provider{Engine: harness.Grok}, WorkDir: t.TempDir()})
	if facts, ok := harness.ErrorFacts(err); !ok || facts.Family != harness.FailureCapability && facts.Family != harness.FailurePreflight {
		t.Fatalf("an unset Grok permission policy was accepted: %v", err)
	}
}

func TestPersistedGrokRefIsStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pinned paths are Unix paths")
	}
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Grok, CLI: harness.CLI{Home: "/h"}}, Model: "grok-4.7", Effort: "low", WorkDir: "/w", Policy: Policy{GrokPermission: GrokDenyWhenAsked}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(reference(o, "s1"))
	want := `{"engine":"grok","id":"s1","home":"/h","work_dir":"/w","config_hash":"477d318ae28c2c79617531ffca468564f209a499bc87835bde6069f0ceeb6350"}`
	if string(got) != want {
		t.Fatalf("persisted ref changed:\n got %s\nwant %s", got, want)
	}
}
