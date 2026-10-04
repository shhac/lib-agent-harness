//go:build !windows

package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// A synthetic ACP agent: this test binary re-executed as `cmd acp`. It answers
// with the shapes Command Code 1.74.1 was observed to use, performs no
// inference and reads no login. It logs what it saw to a file the test reads.
const (
	commandCodeFixtureEnv  = "LIB_HARNESS_COMMAND_CODE_FIXTURE"
	commandCodeFixtureLog  = "LIB_HARNESS_COMMAND_CODE_FIXTURE_LOG"
	commandCodeFixtureMode = "LIB_HARNESS_COMMAND_CODE_FIXTURE_MODE"
	// commandCodeFixtureStubborn makes set_config_option answer without
	// changing anything, as a harness that ignored the request would.
	commandCodeFixtureStubborn = "LIB_HARNESS_COMMAND_CODE_FIXTURE_STUBBORN"
	// commandCodeFixtureSetMode is how set_mode behaves: "" or "report" changes the mode
	// and announces it, "refuse" answers -32602, "silent" answers without
	// changing anything.
	commandCodeFixtureSetMode     = "LIB_HARNESS_COMMAND_CODE_FIXTURE_SET_MODE"
	commandCodeFixtureStartupMode = "LIB_HARNESS_COMMAND_CODE_FIXTURE_STARTUP_MODE"
	commandCodeFixtureListOnce    = "LIB_HARNESS_COMMAND_CODE_FIXTURE_LIST_ONCE"
	// commandCodeFixtureResolve makes session/list name each directory by its
	// resolved path, as Command Code may store it.
	commandCodeFixtureResolve = "LIB_HARNESS_COMMAND_CODE_FIXTURE_RESOLVE"
)

func init() {
	if os.Getenv(commandCodeFixtureEnv) != "1" {
		return
	}
	os.Exit(runCommandCodeFixture())
}

func runCommandCodeFixture() int {
	logFile, err := os.OpenFile(os.Getenv(commandCodeFixtureLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 2
	}
	defer logFile.Close()
	record := func(kind, value string) { _, _ = logFile.WriteString(kind + "\t" + value + "\n") }
	record("args", strings.Join(os.Args[1:], " "))
	record("home", os.Getenv("HOME"))
	record("codex_home", os.Getenv("CODEX_HOME"))
	record("protective", os.Getenv("CMD_ZDR")+","+os.Getenv("CMD_LOCAL_ONLY"))
	record("inherited", os.Getenv("CMD_UNMANAGED")+","+os.Getenv("COMMANDCODE_SKIP_UPDATES")+","+os.Getenv("COMMAND_CODE_TELEMETRY_SYNC"))
	mode := os.Getenv(commandCodeFixtureMode)
	if mode == "" {
		mode = "default"
	}
	stubborn := os.Getenv(commandCodeFixtureStubborn) == "1"
	model, effort := "fixture-default", "high"
	models := []string{"fixture-default", "fixture-fast", "fixture-plain"}
	efforts := func() []string {
		if model == "fixture-plain" {
			return nil
		}
		return []string{"off", "high", "max"}
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
	startupMode := func(session string) {
		if os.Getenv(commandCodeFixtureStartupMode) == "1" {
			mode = "bypass"
			update(session, map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": mode})
		}
	}
	config := func() []any {
		var modelOptions []any
		for _, m := range models {
			modelOptions = append(modelOptions, map[string]any{"value": m, "name": m})
		}
		options := []any{map[string]any{"id": "model", "category": "model", "type": "select", "currentValue": model, "options": modelOptions}}
		if available := efforts(); available != nil {
			var effortOptions []any
			for _, e := range available {
				effortOptions = append(effortOptions, map[string]any{"value": e, "name": e})
			}
			options = append(options, map[string]any{"id": "effort", "category": "thought_level", "type": "select", "currentValue": effort, "options": effortOptions})
		}
		return options
	}
	sessionState := func() map[string]any {
		var available []any
		for _, m := range []string{"default", "auto-accept", "plan", "dont-ask", "bypass"} {
			if m == "default" && mode == "no-default" {
				continue
			}
			available = append(available, map[string]any{"id": m, "name": m})
		}
		return map[string]any{"modes": map[string]any{"currentModeId": mode, "availableModes": available}, "configOptions": config()}
	}
	onceOrAlways := []any{
		map[string]any{"optionId": "allow_once", "kind": "allow_once"}, map[string]any{"optionId": "allow_always", "kind": "allow_always"},
		map[string]any{"optionId": "reject_once", "kind": "reject_once"}, map[string]any{"optionId": "reject_always", "kind": "reject_always"}}
	permission := func(session, kind string, options []any) (string, bool) {
		send(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/request_permission", "params": map[string]any{"sessionId": session, "toolCall": map[string]any{"toolCallId": "call_1", "kind": kind}, "options": options}})
		answer, ok := next()
		if !ok || string(answer["id"]) != "0" {
			return "", false
		}
		var outcome struct {
			Outcome struct{ Outcome, OptionID string } `json:"outcome"`
		}
		_ = json.Unmarshal(answer["result"], &outcome)
		return outcome.Outcome.Outcome + ":" + outcome.Outcome.OptionID, true
	}
	listCalls := 0
	for {
		m, ok := next()
		if !ok {
			return 0
		}
		id := m["id"]
		reply := func(result any) bool { return send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		refuse := func(code int, message string) bool {
			return send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
		}
		var p map[string]json.RawMessage
		_ = json.Unmarshal(m["params"], &p)
		switch method := str(m, "method"); method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}}, "authMethods": []any{map[string]any{"id": "command-code-cli"}}})
		case "session/new":
			record("new", string(m["params"]))
			state := sessionState()
			state["sessionId"] = "cc-session-1"
			reply(state)
			startupMode("cc-session-1")
		case "session/list":
			record("list", string(m["params"]))
			listCalls++
			cwd := str(p, "cwd")
			if os.Getenv(commandCodeFixtureListOnce) == "1" {
				sessions := []any{}
				if listCalls == 1 {
					sessions = append(sessions, map[string]any{"sessionId": "cc-session-1", "cwd": cwd})
				}
				reply(map[string]any{"sessions": sessions})
				continue
			}
			if os.Getenv(commandCodeFixtureResolve) == "1" {
				if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
					cwd = resolved
				}
			}
			// The conversation is on the second page, behind another one.
			if str(p, "cursor") == "" {
				reply(map[string]any{"sessions": []any{map[string]any{"sessionId": "cc-session-other", "cwd": cwd}}, "nextCursor": "page-2"})
				continue
			}
			reply(map[string]any{"sessions": []any{map[string]any{"sessionId": "cc-session-1", "cwd": cwd}, map[string]any{"sessionId": "cc-session-elsewhere", "cwd": "/elsewhere"}}})
		case "session/resume":
			record("resume", string(m["params"]))
			reply(sessionState())
			startupMode(str(p, "sessionId"))
		case "session/close":
			record("close", str(p, "sessionId"))
			reply(map[string]any{})
		case "session/set_mode":
			record("mode", str(p, "modeId"))
			switch os.Getenv(commandCodeFixtureSetMode) {
			case "refuse":
				refuse(-32602, "Unknown mode: secret")
				continue
			case "silent":
			default:
				// As Command Code 1.74.1 does, announce only a change.
				if str(p, "modeId") != mode {
					mode = str(p, "modeId")
					update(str(p, "sessionId"), map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": mode})
				}
			}
			reply(map[string]any{})
		case "session/set_config_option":
			option, value := str(p, "configId"), str(p, "value")
			record("config", option+"="+value)
			switch {
			case option == "model" && !slices.Contains(models, value):
				refuse(-32602, "Unknown model: "+value)
				continue
			case option == "effort" && efforts() == nil:
				refuse(-32601, "The current model has no reasoning effort setting")
				continue
			case option == "effort" && !slices.Contains(efforts(), value):
				refuse(-32602, "Unknown effort: "+value)
				continue
			}
			if !stubborn {
				if option == "model" {
					model = value
				} else {
					effort = value
				}
			}
			reply(map[string]any{"configOptions": config()})
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
				update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "title": "Write: /secret/path", "kind": "edit", "status": "pending", "rawInput": map[string]any{"file_path": "/secret/path", "content": "hi"}})
				answer, ok := permission(session, "edit", onceOrAlways)
				if !ok {
					return 3
				}
				record("permission", answer)
				status := "failed"
				if answer == "selected:allow_once" {
					status = "completed"
				}
				update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": status})
				reply(map[string]any{"stopReason": "end_turn"})
			case strings.HasSuffix(text, "switch"):
				answer, ok := permission(session, "switch_mode", onceOrAlways)
				if !ok {
					return 3
				}
				record("switch", answer)
				reply(map[string]any{"stopReason": "end_turn"})
			case strings.HasSuffix(text, "drift"):
				// The agent leaves the asking mode by itself, mid-turn.
				update(session, map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "bypass"})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "editing freely"}})
				reply(map[string]any{"stopReason": "end_turn"})
			case strings.HasSuffix(text, "question"):
				answer, ok := permission(session, "think", []any{map[string]any{"optionId": "option_0", "kind": "allow_once"}, map[string]any{"optionId": "option_1", "kind": "allow_once"}})
				if !ok {
					return 3
				}
				record("question", answer)
				reply(map[string]any{"stopReason": "end_turn"})
			case strings.HasSuffix(text, "wait"):
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "working"}})
				cancel, ok := next()
				if !ok || str(cancel, "method") != "session/cancel" || len(cancel["id"]) != 0 {
					return 3
				}
				record("cancel", "")
				reply(map[string]any{"stopReason": "cancelled"})
			case strings.HasSuffix(text, "fail"):
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "secret provider text", "data": map[string]any{"status": 429, "code": "secret"}}})
			default:
				update(session, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "thinking"}})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Let me look. "}})
				update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "title": "List: /secret/path", "kind": "read", "status": "pending", "rawInput": map[string]any{"path": "/secret/path"}})
				update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "in_progress"})
				update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed", "content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "Found 1 items"}}}})
				update("another-session", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "not ours"}})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "fixture "}})
				update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "answer"}})
				// An agent request the client never offered must be refused, not ignored.
				send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "fs/read_text_file", "params": map[string]any{"sessionId": session, "path": "/etc/passwd"}})
				answer, ok := next()
				if !ok || string(answer["id"]) != "7" || len(answer["error"]) == 0 {
					return 3
				}
				record("refused", "fs/read_text_file")
				update(session, map[string]any{"sessionUpdate": "usage_update", "used": 120, "size": 1000, "cost": map[string]any{"amount": 0, "currency": "USD"}})
				reply(map[string]any{"stopReason": "end_turn", "usage": map[string]any{"totalTokens": 900, "inputTokens": 850, "outputTokens": 50, "cachedReadTokens": 700, "cachedWriteTokens": 0}, "_meta": map[string]any{"usage": map[string]any{"inputTokens": 85, "outputTokens": 6, "cacheReadTokens": 70, "cacheWriteTokens": 0}}})
			}
		default:
			if len(id) != 0 {
				refuse(-32601, "Method not found")
			}
		}
	}
}

// commandCodeFixtureOptions runs the fixture as `cmd` under a temporary HOME,
// which is where Command Code keeps its configuration.
func commandCodeFixtureOptions(t *testing.T) (Options, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fixture.log")
	t.Setenv(commandCodeFixtureEnv, "1")
	t.Setenv(commandCodeFixtureLog, log)
	t.Setenv("HOME", t.TempDir())
	return Options{Provider: harness.Provider{Engine: harness.CommandCode, CLI: harness.CLI{Binary: os.Args[0]}}, WorkDir: t.TempDir()}, log
}

func TestCommandCodeSessionOverTheAgentProtocol(t *testing.T) {
	o, log := commandCodeFixtureOptions(t)
	t.Setenv("CMD_UNMANAGED", "1")
	t.Setenv("COMMANDCODE_SKIP_UPDATES", "1")
	t.Setenv("COMMAND_CODE_TELEMETRY_SYNC", "1")
	o.Model, o.Effort = "fixture-fast", "max"
	ctx := testContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	if ref.Engine != harness.CommandCode || ref.ID != "cc-session-1" || ref.Home != filepath.Join(os.Getenv("HOME"), ".commandcode") {
		t.Fatalf("reference: %+v", ref)
	}
	if caps := s.Capabilities(); caps.Start.Availability != harness.Native || caps.AppendInstructions.Usable() {
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
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		if e.Kind == "tool_started" && (e.Tool != "read" || e.ItemID != "call_1") {
			t.Errorf("tool named from its title or misidentified: %+v", e)
		}
		if e.Kind == "tool_completed" && (e.Status != "completed" || e.ItemID != "call_1") {
			t.Errorf("tool completion: %+v", e)
		}
		if e.Kind == "text_delta" && strings.Contains(e.Text, "not ours") {
			t.Error("another session's update reached this turn")
		}
	}
	if slices.Index(kinds, "tool_started") < 0 || slices.Index(kinds, "tool_completed") < 0 || kinds[len(kinds)-1] != "status" {
		t.Fatalf("events: %v", kinds)
	}
	// The turn's own figures, not the session's running total.
	final := result.Usage
	if !final.Final || final.Input != 85 || final.CacheRead != 70 || final.Output != 6 || !final.CacheKnown {
		t.Fatalf("turn accounting: %+v", final)
	}
	if fresh, ok := final.Fresh(); !ok || fresh != 15 {
		t.Fatalf("fresh input %d %v", fresh, ok)
	}
	c := result.Context
	if c.Quality != harness.Estimated || c.UsedTokens == nil || *c.UsedTokens != 120 || c.CapacityTokens == nil || *c.CapacityTokens != 1000 || c.Model != "fixture-fast" {
		t.Fatalf("context: %+v", c)
	}
	if _, err = s.ReadAccount(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("account: %v", err)
	}
	if _, err = s.ReadQuota(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("quota: %v", err)
	}
	if _, err = s.Compact(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("compact: %v", err)
	}
	s.Close()

	logged := grokFixtureLogged(t, log)
	if logged["args"][0] != "acp" {
		t.Errorf("arguments: %q", logged["args"])
	}
	// No other engine's home variable is set on Command Code's behalf.
	if logged["home"][0] != os.Getenv("HOME") || logged["codex_home"][0] != "" {
		t.Errorf("environment: home %q codex home %q", logged["home"], logged["codex_home"])
	}
	// Command Code's managed variables are no more inherited than accepted.
	if logged["inherited"][0] != ",," {
		t.Errorf("inherited Command Code variables: %q", logged["inherited"])
	}
	var created struct {
		Cwd  string         `json:"cwd"`
		MCP  []any          `json:"mcpServers"`
		Meta map[string]any `json:"_meta"`
	}
	if json.Unmarshal([]byte(logged["new"][0]), &created) != nil || created.Cwd != o.WorkDir || created.MCP == nil || len(created.MCP) != 0 || created.Meta != nil {
		t.Errorf("session/new: %s", logged["new"])
	}
	if !slices.Equal(logged["config"], []string{"model=fixture-fast", "effort=max"}) || !slices.Equal(logged["mode"], []string{"default"}) {
		t.Errorf("configuration: %q mode %q", logged["config"], logged["mode"])
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
	logged = grokFixtureLogged(t, log)
	var resumedWith struct {
		Session string `json:"sessionId"`
		Cwd     string `json:"cwd"`
	}
	if len(logged["list"]) != 4 || json.Unmarshal([]byte(logged["resume"][0]), &resumedWith) != nil || resumedWith.Session != "cc-session-1" || resumedWith.Cwd != o.WorkDir {
		t.Errorf("session/list %q, session/resume %q", logged["list"], logged["resume"])
	}
	// A resumed process starts from Command Code's defaults, so the model and
	// effort are applied again.
	if !slices.Equal(logged["config"], []string{"model=fixture-fast", "effort=max", "model=fixture-fast", "effort=max"}) {
		t.Errorf("resumed configuration: %q", logged["config"])
	}
	if !slices.Equal(logged["mode"], []string{"default", "default"}) {
		t.Errorf("resumed mode: %q", logged["mode"])
	}
}

func TestCommandCodeProtectiveEnvironment(t *testing.T) {
	for _, source := range []string{"inherited", "additions"} {
		t.Run(source, func(t *testing.T) {
			o, log := commandCodeFixtureOptions(t)
			t.Setenv("CMD_ZDR", "1")
			t.Setenv("CMD_LOCAL_ONLY", "1")
			t.Setenv("CMD_UNMANAGED", "1")
			t.Setenv("COMMANDCODE_SKIP_UPDATES", "1")
			t.Setenv("COMMAND_CODE_TELEMETRY_SYNC", "1")
			want := "1,1"
			if source == "additions" {
				o.Env = []string{"CMD_ZDR=true", "CMD_LOCAL_ONLY=true"}
				want = "true,true"
			}
			s, err := Start(testContext(t), o)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			logged := grokFixtureLogged(t, log)
			if !slices.Equal(logged["protective"], []string{want}) {
				t.Errorf("protective environment: %q", logged["protective"])
			}
			if !slices.Equal(logged["inherited"], []string{",,"}) {
				t.Errorf("managed environment: %q", logged["inherited"])
			}
			for _, entry := range []string{"CMD_UNMANAGED=1", "COMMANDCODE_SKIP_UPDATES=1", "COMMAND_CODE_TELEMETRY_SYNC=1"} {
				o.Env = []string{entry}
				_, err := normalize(o)
				var refusal *UnsupportedError
				if !errors.As(err, &refusal) || refusal.Code != RefusedEnvManaged {
					t.Errorf("managed addition %s: %v", entry, err)
				}
			}
		})
	}
}

// Command Code resumes an id it does not have as a new, empty conversation,
// without failing, so a conversation session/list does not hold is gone.
func TestCommandCodeOpenStartsFreshWhenTheConversationIsGone(t *testing.T) {
	o, log := commandCodeFixtureOptions(t)
	ctx := testContext(t)
	n, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cc-session-gone", "cc-session-elsewhere"} {
		gone := reference(n, id)
		if _, err = Resume(ctx, o, gone); !errors.Is(err, errConversationGone) {
			t.Fatalf("%s resumed: %v", id, err)
		}
		s, opened, err := Open(ctx, o, &gone)
		if err != nil {
			t.Fatal(err)
		}
		if opened.Fresh != FreshUnavailable || s.Ref().ID != "cc-session-1" {
			t.Fatalf("%+v %+v", opened, s.Ref())
		}
		s.Close()
	}
	if got := grokFixtureLogged(t, log)["resume"]; len(got) != 0 {
		t.Fatalf("a conversation Command Code does not hold was resumed: %q", got)
	}
}

func TestCommandCodeConversationDisappearsDuringResume(t *testing.T) {
	for _, operation := range []string{"Resume", "Open"} {
		t.Run(operation, func(t *testing.T) {
			o, log := commandCodeFixtureOptions(t)
			t.Setenv(commandCodeFixtureListOnce, "1")
			ctx := testContext(t)
			n, err := normalize(o)
			if err != nil {
				t.Fatal(err)
			}
			ref := reference(n, "cc-session-1")
			if operation == "Open" {
				s, opened, err := Open(ctx, o, &ref)
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
				if opened.Resumed || opened.Fresh != FreshUnavailable || s.Ref().ID != "cc-session-1" {
					t.Fatalf("%+v %+v", opened, s.Ref())
				}
			} else {
				s, err := Resume(ctx, o, ref)
				if s != nil {
					s.Close()
				}
				if !errors.Is(err, errConversationGone) {
					t.Fatalf("a vanished conversation was resumed: %v", err)
				}
			}
			logged := grokFixtureLogged(t, log)
			if len(logged["list"]) != 2 || len(logged["resume"]) != 1 || !slices.Equal(logged["close"], []string{"cc-session-1"}) {
				t.Fatalf("list %q resume %q close %q", logged["list"], logged["resume"], logged["close"])
			}
			if operation == "Open" && len(logged["new"]) != 1 {
				t.Fatalf("a fresh conversation was not created: %q", logged["new"])
			}
		})
	}
}

func TestCommandCodeStartupModeUpdate(t *testing.T) {
	for _, operation := range []string{"start", "resume"} {
		for _, setMode := range []string{"report", "refuse", "silent"} {
			t.Run(operation+"/"+setMode, func(t *testing.T) {
				o, log := commandCodeFixtureOptions(t)
				t.Setenv(commandCodeFixtureMode, "default")
				t.Setenv(commandCodeFixtureStartupMode, "1")
				t.Setenv(commandCodeFixtureSetMode, setMode)
				ctx := testContext(t)
				var s *Session
				var err error
				if operation == "resume" {
					n, normalizeErr := normalize(o)
					if normalizeErr != nil {
						t.Fatal(normalizeErr)
					}
					s, err = Resume(ctx, o, reference(n, "cc-session-1"))
				} else {
					s, err = Start(ctx, o)
				}
				if s != nil {
					defer s.Close()
				}
				if setMode != "report" {
					var failure *CapabilityError
					if !errors.As(err, &failure) || failure.Code != CapabilityChangedPermissionMode || failure.Phase != BeforeFirstPrompt {
						t.Fatalf("a startup mode change was accepted: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					s.mu.Lock()
					mode := s.commandCodeMode
					s.mu.Unlock()
					if mode != commandCodeAskingMode {
						t.Fatalf("mode: %q", mode)
					}
				}
				if got := grokFixtureLogged(t, log)["mode"]; !slices.Equal(got, []string{"default"}) {
					t.Fatalf("the asking mode was not requested: %q", got)
				}
			})
		}
	}
}

func TestCommandCodeModeUpdateBeforeTheWatchIsArmed(t *testing.T) {
	for _, setMode := range []string{"report", "refuse", "silent"} {
		t.Run(setMode, func(t *testing.T) {
			s := &Session{options: Options{Provider: harness.Provider{Engine: harness.CommandCode}}}
			// The reader consumed the update before configuration could arm its watch.
			notify(s, `{"method":"session/update","params":{"sessionId":"cc-session-1","update":{"sessionUpdate":"current_mode_update","currentModeId":"bypass"}}}`)
			w := &fakeWire{requestFn: func(method string, p map[string]any) (json.RawMessage, error) {
				if method != "session/set_mode" || p["sessionId"] != "cc-session-1" || p["modeId"] != commandCodeAskingMode {
					t.Fatalf("unexpected request: %s %v", method, p)
				}
				switch setMode {
				case "refuse":
					return nil, &commandCodeRefusal{rpc: -32602, code: commandCodeInvalidParams}
				case "silent":
				default:
					notify(s, `{"method":"session/update","params":{"sessionId":"cc-session-1","update":{"sessionUpdate":"current_mode_update","currentModeId":"default"}}}`)
				}
				return json.RawMessage(`{}`), nil
			}}
			s.transport = w
			err := s.configureCommandCode(testContext(t), "cc-session-1", commandCodeSessionState{mode: "default", modes: []string{"default", "bypass"}, config: commandCodeConfig{model: "fixture-default"}})
			if setMode == "report" {
				if err != nil || s.commandCodeMode != commandCodeAskingMode || !s.commandCodeConfigured {
					t.Fatalf("mode %q configured %v: %v", s.commandCodeMode, s.commandCodeConfigured, err)
				}
			} else {
				var failure *CapabilityError
				if !errors.As(err, &failure) || failure.Code != CapabilityChangedPermissionMode || failure.Phase != BeforeFirstPrompt || s.commandCodeConfigured {
					t.Fatalf("a stale reply mode was trusted: %v", err)
				}
			}
			if !slices.Equal(w.calls, []string{"session/set_mode"}) {
				t.Fatalf("requests: %q", w.calls)
			}
		})
	}
}

// Every session runs in Command Code's Standard mode, which asks before each
// change, whatever mode the operator's configuration starts it in.
func TestCommandCodeSessionRunsInTheAskingMode(t *testing.T) {
	o, log := commandCodeFixtureOptions(t)
	t.Setenv(commandCodeFixtureMode, "bypass")
	s, err := Start(testContext(t), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := grokFixtureLogged(t, log)["mode"]; len(got) != 1 || got[0] != "default" {
		t.Fatalf("mode: %q", got)
	}
}

// A session that cannot be put in the asking mode, or that does not report
// being in it, is refused before the first prompt and its conversation closed.
func TestCommandCodeAskingModeMustBeReported(t *testing.T) {
	for _, tc := range []struct{ name, mode, setMode string }{
		{"no asking mode", "no-default", ""},
		{"set_mode refused", "bypass", "refuse"},
		{"set_mode ignored", "bypass", "silent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, log := commandCodeFixtureOptions(t)
			t.Setenv(commandCodeFixtureMode, tc.mode)
			t.Setenv(commandCodeFixtureSetMode, tc.setMode)
			_, err := Start(testContext(t), o)
			var failure *CapabilityError
			if !errors.As(err, &failure) || failure.Code != CapabilityChangedPermissionMode || failure.Phase != BeforeFirstPrompt {
				t.Fatalf("%v", err)
			}
			if got := grokFixtureLogged(t, log)["close"]; len(got) != 1 || got[0] != "cc-session-1" {
				t.Fatalf("the refused session was not closed: %q", got)
			}
		})
	}
}

// Leaving the asking mode would let every later change run without asking,
// so a session never approves it, and one that leaves anyway is stopped.
func TestCommandCodeNeverLeavesTheAskingMode(t *testing.T) {
	o, log := commandCodeFixtureOptions(t)
	o.Policy.CommandCodePermission = CommandCodeAllowWhenAsked
	ctx := testContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	turn, err := s.StartTurn(ctx, Input{Text: "switch"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	if result, err := turn.Wait(ctx); err != nil || result.Status != "completed" {
		t.Fatalf("%+v %v", result, err)
	}
	if got := grokFixtureLogged(t, log)["switch"]; len(got) != 1 || got[0] != "selected:reject_once" {
		t.Fatalf("a mode switch was answered %q", got)
	}
	turn, err = s.StartTurn(ctx, Input{Text: "drift"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	result, err := turn.Wait(ctx)
	var failure *CapabilityError
	if result.Status != "failed" || !errors.As(err, &failure) || failure.Code != CapabilityChangedPermissionMode || failure.Phase != DuringSession {
		t.Fatalf("a session left the asking mode: %+v %v", result, err)
	}
	if _, err = s.StartTurn(ctx, Input{Text: "hello"}); err == nil {
		t.Fatal("the session kept running outside the asking mode")
	}
}

// Command Code may list a conversation's directory by its resolved path.
func TestCommandCodeResumeMatchesTheResolvedDirectory(t *testing.T) {
	o, log := commandCodeFixtureOptions(t)
	t.Setenv(commandCodeFixtureResolve, "1")
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	o.WorkDir = link
	ctx := testContext(t)
	n, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	ref := reference(n, "cc-session-1")
	s, opened, err := Open(ctx, o, &ref)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !opened.Resumed || s.Ref().ID != "cc-session-1" {
		t.Fatalf("%+v %+v", opened, s.Ref())
	}
	if got := grokFixtureLogged(t, log)["resume"]; len(got) != 1 {
		t.Fatalf("resumed %q", got)
	}
}

func TestCommandCodePermissionRequestsFollowThePolicy(t *testing.T) {
	for _, tc := range []struct {
		policy, answer, tool string
	}{
		{"", "selected:reject_once", "failed"},
		{CommandCodeDenyWhenAsked, "selected:reject_once", "failed"},
		{CommandCodeAllowWhenAsked, "selected:allow_once", "completed"},
	} {
		t.Run("policy "+tc.policy, func(t *testing.T) {
			o, log := commandCodeFixtureOptions(t)
			o.Policy.CommandCodePermission = tc.policy
			ctx := testContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, prompt := range []string{"permission", "question"} {
				turn, err := s.StartTurn(ctx, Input{Text: prompt})
				if err != nil {
					t.Fatal(err)
				}
				events := drain(t, turn)
				if result, err := turn.Wait(ctx); err != nil || result.Status != "completed" {
					t.Fatalf("%+v %v", result, err)
				}
				for _, e := range events {
					if e.Kind == "tool_completed" && e.Status != tc.tool {
						t.Errorf("tool status %q", e.Status)
					}
				}
			}
			logged := grokFixtureLogged(t, log)
			if got := logged["permission"]; len(got) != 1 || got[0] != tc.answer {
				t.Fatalf("permission answered %q", got)
			}
			// A question to the user is not a permission: no answer is guessed.
			if got := logged["question"]; len(got) != 1 || got[0] != "cancelled:" {
				t.Fatalf("question answered %q", got)
			}
		})
	}
}

func TestCommandCodePermissionNeverGrantsMoreThanOnce(t *testing.T) {
	for _, tc := range []string{
		`{"options":[{"optionId":"allow_always","kind":"allow_always"},{"optionId":"reject_always","kind":"reject_always"}]}`,
		`{"options":[{"optionId":"allow_once","kind":"allow_once"}]}`,
		`{"options":[{"optionId":"reject_once","kind":"reject_once"}]}`,
		`{"options":[{"optionId":"a","kind":"allow_once"},{"optionId":"b","kind":"allow_once"},{"optionId":"r","kind":"reject_once"}]}`,
		`{"options":[{"optionId":"same","kind":"allow_once"},{"optionId":"same","kind":"reject_once"}]}`,
		`not json`,
	} {
		for _, policy := range []string{CommandCodeDenyWhenAsked, CommandCodeAllowWhenAsked} {
			if got := commandCodePermissionOutcome(json.RawMessage(tc), policy); got["outcome"] != "cancelled" {
				t.Errorf("%s under %s: %v", tc, policy, got)
			}
		}
	}
	modeSwitch := json.RawMessage(`{"toolCall":{"kind":"switch_mode"},"options":[{"optionId":"allow_once","kind":"allow_once"},{"optionId":"reject_once","kind":"reject_once"}]}`)
	for _, policy := range []string{CommandCodeDenyWhenAsked, CommandCodeAllowWhenAsked} {
		if got := commandCodePermissionOutcome(modeSwitch, policy); got["optionId"] != "reject_once" {
			t.Errorf("a mode switch under %s: %v", policy, got)
		}
	}
	reply := commandCodeServerReply(map[string]json.RawMessage{"id": json.RawMessage(`3`), "method": json.RawMessage(`"terminal/create"`)}, CommandCodeAllowWhenAsked)
	if reply["error"] == nil || reply["result"] != nil || reply["jsonrpc"] != "2.0" {
		t.Errorf("an unhandled request was not refused: %v", reply)
	}
}

func TestCommandCodeInterruptAndComposedSteer(t *testing.T) {
	o, log := commandCodeFixtureOptions(t)
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

func TestCommandCodeRefusedPromptFailsOnlyItsTurn(t *testing.T) {
	o, _ := commandCodeFixtureOptions(t)
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
	if !errors.As(err, &failure) || failure.Code != commandCodeRateLimited || result.Usage.Known || !result.NativeError {
		t.Fatalf("%+v %v", result, err)
	}
	if facts, ok := harness.ErrorFacts(err); !ok || facts.Cause != harness.CauseRateLimited || facts.Family != harness.FailureTurn {
		t.Fatalf("the refusal's cause was lost: %+v", facts)
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

// A model or effort the session does not end up running is refused before
// the first prompt, and the empty conversation is closed.
func TestCommandCodeConfigurationMustBeHonoured(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort string
		stubborn            bool
		code                string
	}{
		{"unknown model", "fixture-missing", "", false, CapabilityChangedModel},
		{"unknown effort", "", "extreme", false, CapabilityChangedEffort},
		{"model without effort", "fixture-plain", "high", false, CapabilityChangedEffort},
		{"ignored model", "fixture-fast", "", true, CapabilityChangedModel},
		{"ignored effort", "", "max", true, CapabilityChangedEffort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, log := commandCodeFixtureOptions(t)
			if tc.stubborn {
				t.Setenv(commandCodeFixtureStubborn, "1")
			}
			o.Model, o.Effort = tc.model, tc.effort
			_, err := Start(testContext(t), o)
			var failure *CapabilityError
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Phase != BeforeFirstPrompt || failure.Engine != harness.CommandCode {
				t.Fatalf("%v", err)
			}
			if got := grokFixtureLogged(t, log)["close"]; len(got) != 1 || got[0] != "cc-session-1" {
				t.Fatalf("the refused session was not closed: %q", got)
			}
		})
	}
}

// What cmd acp cannot carry is refused before anything is launched.
func TestCommandCodeRefusesWhatItCannotCarry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(t.TempDir(), "guide")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: guide\ndescription: a guide\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Options{Provider: harness.Provider{Engine: harness.CommandCode, CLI: harness.CLI{Binary: filepath.Join(t.TempDir(), "not-installed")}}, WorkDir: t.TempDir()}
	for name, mutate := range map[string]func(*Options){
		"append instructions":  func(o *Options) { o.Instructions = Instructions{Mode: Append, Text: "be brief"} },
		"replace instructions": func(o *Options) { o.Instructions = Instructions{Mode: Replace, Text: "be brief"} },
		"provided skills":      func(o *Options) { o.Skills.Provided = []harness.Skill{{Name: "guide", Dir: skill}} },
		"excluded skills":      func(o *Options) { o.Skills.Global = harness.GlobalSkillsExclude },
		"restriction":          func(o *Options) { o.Restriction = &Restriction{} },
		"sandbox":              func(o *Options) { o.Sandbox = &Sandbox{} },
		"browser":              func(o *Options) { o.Browser = true },
		"another home":         func(o *Options) { o.Provider.CLI.Home = filepath.Join(home, "elsewhere") },
		"grok policy":          func(o *Options) { o.Policy.GrokPermission = GrokAllowWhenAsked },
		"claude policy":        func(o *Options) { o.Policy.ClaudePermission = "dontAsk" },
		"unknown permission":   func(o *Options) { o.Policy.CommandCodePermission = "always" },
		"managed environment":  func(o *Options) { o.Env = []string{"CMD_UNMANAGED=1"} },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			_, err := Start(testContext(t), o)
			if err == nil || errors.Is(err, ErrTransport) {
				t.Fatalf("not refused before launch: %v", err)
			}
			if facts, ok := harness.ErrorFacts(err); !ok || facts.Engine != harness.CommandCode {
				t.Fatalf("facts %+v %v: %v", facts, ok, err)
			}
		})
	}
	n, err := normalize(Options{Provider: harness.Provider{Engine: harness.CommandCode, CLI: harness.CLI{Home: filepath.Join(home, ".commandcode")}}, WorkDir: t.TempDir()})
	if err != nil || n.Provider.CLI.Binary != "cmd" || n.Policy.CommandCodePermission != CommandCodeDenyWhenAsked {
		t.Fatalf("the default home was refused or defaults changed: %+v %v", n, err)
	}
	codex := Options{Provider: harness.Provider{Engine: harness.Codex}, WorkDir: t.TempDir(), Policy: Policy{CommandCodePermission: CommandCodeAllowWhenAsked}}
	if _, err = normalize(codex); err == nil {
		t.Fatal("Codex accepted a Command Code policy it would ignore")
	}
}

func TestCommandCodeStopReasonsAndUsage(t *testing.T) {
	for _, tc := range []struct {
		stop         string
		interrupted  bool
		status, code string
	}{
		{"end_turn", false, "completed", ""},
		{"cancelled", true, "interrupted", ""},
		{"cancelled", false, "failed", commandCodeCancelled},
		{"max_tokens", false, "failed", "max_tokens"},
		{"max_turn_requests", false, "failed", "max_turn_requests"},
		{"something_new", false, "failed", commandCodeStopUnrecognized},
	} {
		status, code := commandCodeOutcome(tc.stop, tc.interrupted)
		if status != tc.status || code != tc.code {
			t.Errorf("%+v: %s %s", tc, status, code)
		}
	}
	// Observed from Command Code 1.74.1: inputTokens already counts the cache.
	turn := parseCommandCodeTurnUsage(json.RawMessage(`{"inputTokens":44375,"outputTokens":485,"cacheReadTokens":29888,"cacheWriteTokens":0}`))
	if !turn.Known || turn.Input != 44375 || turn.CacheRead != 29888 || !turn.CacheKnown {
		t.Fatalf("%+v", turn)
	}
	if u := parseCommandCodeTurnUsage(json.RawMessage(`{"inputTokens":10,"outputTokens":1}`)); !u.Known || u.CacheKnown {
		t.Fatalf("a missing cache split was claimed: %+v", u)
	}
	for _, raw := range []string{``, `{}`, `{"inputTokens":10}`, `{"inputTokens":10,"outputTokens":1,"cacheReadTokens":11}`, `{"inputTokens":-1,"outputTokens":1}`, `{"inputTokens":10,"outputTokens":1,"cacheReadTokens":6,"cacheWriteTokens":5}`} {
		if u := parseCommandCodeTurnUsage(json.RawMessage(raw)); u.Known {
			t.Errorf("%s read as known: %+v", raw, u)
		}
	}
}

func TestCommandCodeRefusalsAreClassifiedWithoutProviderText(t *testing.T) {
	for _, tc := range []struct {
		reply string
		code  string
		is    error
	}{
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"secret"}}`, commandCodeMethodMissing, ErrUnsupported},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32602,"message":"Unknown model: secret"}}`, commandCodeInvalidParams, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32600,"message":"A prompt is already running"}}`, commandCodeBusy, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32000,"message":"Not authenticated. secret"}}`, commandCodeAuthRequired, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"secret","data":{"status":401,"code":"secret"}}}`, commandCodeAuthFailed, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"secret","data":{"status":503}}}`, commandCodeProviderUnavailable, ErrRejected},
		{`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"secret"}}`, commandCodeRejected, ErrRejected},
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.reply), &m); err != nil {
			t.Fatal(err)
		}
		id, r, isReply, err := parseCommandCodeReply(m)
		var refusal *commandCodeRefusal
		if id != "1" || !isReply || err != nil || !errors.As(r.err, &refusal) || refusal.code != tc.code || !errors.Is(r.err, tc.is) {
			t.Fatalf("%s: %q %+v %v %v", tc.reply, id, r, isReply, err)
		}
		if strings.Contains(r.err.Error(), "secret") {
			t.Fatal("provider text reached the error")
		}
		if facts, ok := harness.ErrorFacts(r.err); !ok || facts.Code != tc.code || facts.Engine != harness.CommandCode {
			t.Fatalf("facts: %+v %v", facts, ok)
		}
	}
}

// Command Code references are persisted like any other; this pins their form.
func TestPersistedCommandCodeRefIsStable(t *testing.T) {
	t.Setenv("HOME", "/h")
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.CommandCode}, Model: "fixture-fast", Effort: "max", WorkDir: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(reference(o, "s1"))
	want := `{"engine":"command-code","id":"s1","home":"/h/.commandcode","work_dir":"/w","config_hash":"91924ce1ae63d3ce727be8994626afb605cfa3139a2687800723e7fe28cd2944"}`
	if string(got) != want {
		t.Fatalf("persisted ref changed:\n got %s\nwant %s", got, want)
	}
	allow := o
	allow.Policy.CommandCodePermission = CommandCodeAllowWhenAsked
	if compatible(allow, reference(o, "s1")) {
		t.Fatal("the permission policy is not part of the reference")
	}
}
