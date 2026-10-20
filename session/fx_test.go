package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// A synthetic fx agent: this test binary re-executed as `fx acp`. It answers
// with the shapes fx 0.0.14 was observed to use, performs no inference and
// reads no login. It logs what it saw to a file the test reads.
const (
	fxFixtureEnv = "LIB_HARNESS_FX_FIXTURE"
	fxFixtureLog = "LIB_HARNESS_FX_FIXTURE_LOG"
	// fxFixtureNoMode makes the fixture behave like an fx that does not
	// report the session's permission mode.
	fxFixtureNoMode = "LIB_HARNESS_FX_FIXTURE_NO_MODE"
	// fxFixtureNoAsk makes the fixture behave like an fx whose modes do not
	// include the one that asks.
	fxFixtureNoAsk = "LIB_HARNESS_FX_FIXTURE_NO_ASK"
	// fxFixtureSharedOption makes the fixture offer two permission options
	// that share one id, the shape a deny must not be read from.
	fxFixtureSharedOption = "LIB_HARNESS_FX_FIXTURE_SHARED_OPTION"
)

func init() {
	if os.Getenv(fxFixtureEnv) != "1" {
		return
	}
	os.Exit(runFxFixture())
}

// runFxFixture is a small ACP agent: it holds one session, answers the
// methods fx answers, and asks the client for a permission when the prompt
// says so.
func runFxFixture() int {
	logFile, err := os.OpenFile(os.Getenv(fxFixtureLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 2
	}
	defer logFile.Close()
	record := func(kind, value string) { _, _ = logFile.WriteString(kind + "\t" + value + "\n") }
	record("args", strings.Join(os.Args[1:], " "))
	record("home", os.Getenv("HOME"))
	record("fx", os.Getenv("FX_ULTRAFAST"))
	record("fxmodel", os.Getenv("FX_MODEL"))

	model, effort, mode := "fx-default", "high", "auto"
	session := "fx-session-1"
	noMode := os.Getenv(fxFixtureNoMode) == "1"
	noAsk := os.Getenv(fxFixtureNoAsk) == "1"
	sharedOption := os.Getenv(fxFixtureSharedOption) == "1"
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
	update := func(u map[string]any) bool {
		return send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": u}})
	}
	options := func() []any {
		list := []any{
			map[string]any{"id": "provider", "currentValue": "gateway", "options": []any{map[string]any{"value": "gateway"}}},
			map[string]any{"id": "model", "currentValue": model, "options": []any{map[string]any{"value": model}}},
		}
		if !noMode {
			list = append(list, map[string]any{"id": "mode", "currentValue": mode, "options": []any{map[string]any{"value": "auto"}, map[string]any{"value": "ask"}, map[string]any{"value": "full-access"}}})
		}
		return append(list, map[string]any{"id": "effort", "currentValue": effort, "options": []any{map[string]any{"value": "low"}, map[string]any{"value": "high"}}})
	}
	modes := func() map[string]any {
		if noMode {
			return map[string]any{}
		}
		available := []any{map[string]any{"id": "auto"}, map[string]any{"id": "ask"}, map[string]any{"id": "full-access"}}
		if noAsk {
			// An fx whose modes do not include the one that asks.
			available = []any{map[string]any{"id": "auto"}, map[string]any{"id": "full-access"}}
		}
		return map[string]any{"currentModeId": mode, "availableModes": available}
	}
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
		switch str(m, "method") {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}}, "agentInfo": map[string]any{"name": "fx", "version": "0.0.14"}})
		case "session/new":
			record("new", string(m["params"]))
			reply(map[string]any{"sessionId": session, "configOptions": options(), "modes": modes()})
		case "session/resume":
			record("resume", string(m["params"]))
			if str(p, "sessionId") != session {
				refuse(-32602, "Session not found")
				continue
			}
			reply(map[string]any{"configOptions": options()})
		case "session/set_config_option":
			var want struct {
				Session string `json:"sessionId"`
				ID      string `json:"configId"`
				Value   string `json:"value"`
			}
			_ = json.Unmarshal(m["params"], &want)
			record("config", want.ID+"="+want.Value)
			switch want.ID {
			case "model":
				if want.Value == "refused-model" {
					// fx keeps the model it had, and reports that back.
					reply(map[string]any{"configOptions": options()})
					continue
				}
				model = want.Value
			case "mode":
				if noMode {
					// An fx that does not report the permission mode.
					refuse(-32602, "Unknown config option")
					continue
				}
				if want.Value != "auto" && want.Value != "ask" && want.Value != "full-access" {
					refuse(-32602, "Unknown mode")
					continue
				}
				mode = want.Value
			case "effort":
				if want.Value != "low" && want.Value != "high" {
					refuse(-32602, "Reasoning effort is unavailable for the active model")
					continue
				}
				effort = want.Value
			default:
				refuse(-32602, "Unknown config option")
				continue
			}
			reply(map[string]any{"configOptions": options()})
		case "session/close":
			record("close", str(p, "sessionId"))
			reply(map[string]any{})
		case "session/prompt":
			var prompt struct {
				Session string `json:"sessionId"`
				Prompt  []struct{ Type, Text string }
			}
			if json.Unmarshal(m["params"], &prompt) != nil || len(prompt.Prompt) != 1 || prompt.Prompt[0].Type != "text" {
				return 3
			}
			text := prompt.Prompt[0].Text
			record("prompt", text)
			switch {
			case strings.HasSuffix(text, "permission"):
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "name": "write_file", "kind": "edit", "status": "pending", "rawInput": map[string]any{"path": "/tmp/x"}})
				options := []any{
					map[string]any{"optionId": "allow_once", "kind": "allow_once"},
					map[string]any{"optionId": "allow_always", "kind": "allow_always"},
					map[string]any{"optionId": "reject_once", "kind": "reject_once"}}
				if sharedOption {
					// Two options that share one id: a deny chosen from them
					// could be read as an allow.
					options = []any{
						map[string]any{"optionId": "shared", "kind": "allow_once"},
						map[string]any{"optionId": "shared", "kind": "reject_once"}}
				}
				send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "session/request_permission", "params": map[string]any{"sessionId": session, "toolCall": map[string]any{"toolCallId": "call_1", "name": "write_file", "kind": "edit"}, "options": options}})
				answer, ok := next()
				if !ok || string(answer["id"]) != "5" {
					return 3
				}
				var outcome struct {
					Outcome struct{ Outcome, OptionID string } `json:"outcome"`
				}
				_ = json.Unmarshal(answer["result"], &outcome)
				record("permission", outcome.Outcome.Outcome+":"+outcome.Outcome.OptionID)
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed"})
				reply(map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 2}})
			case strings.HasSuffix(text, "wait"):
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m1", "content": map[string]any{"type": "text", "text": "working"}})
				cancel, ok := next()
				if !ok || str(cancel, "method") != "session/cancel" || len(cancel["id"]) != 0 {
					return 3
				}
				record("cancel", "")
				reply(map[string]any{"stopReason": "cancelled", "usage": map[string]any{"inputTokens": 4, "outputTokens": 0}})
			default:
				// fx 0.0.14 gives each message its own id, and states its own
				// notices as one before the reply.
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m0", "content": map[string]any{"type": "text", "text": "[context] skill catalog shortened\n"}})
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m1", "content": map[string]any{"type": "text", "text": "Let me look. "}})
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "name": "read_file", "kind": "read", "status": "pending", "rawInput": map[string]any{"path": "README.md"}})
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed", "content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "# lib-agent-harness"}}}})
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m2", "content": map[string]any{"type": "text", "text": "answer"}})
				update(map[string]any{"sessionUpdate": "usage_update", "used": 4096, "size": 200000})
				reply(map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 85, "outputTokens": 6, "cacheReadTokens": 64, "cacheWriteTokens": 0, "reasoningTokens": 1}})
			}
		default:
			if len(id) != 0 {
				refuse(-32601, "Method not found")
			}
		}
	}
}

// fxFixtureLogged reads the fixture's log as kind -> values.
func fxFixtureLogged(t *testing.T, log string) map[string][]string {
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

func fxFixtureOptions(t *testing.T) (Options, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fixture.log")
	t.Setenv(fxFixtureEnv, "1")
	t.Setenv(fxFixtureLog, log)
	return Options{Provider: harness.Provider{Engine: harness.Fx, CLI: harness.CLI{Binary: os.Args[0]}}, WorkDir: t.TempDir()}, log
}

func TestFxSessionOverTheAgentProtocol(t *testing.T) {
	o, log := fxFixtureOptions(t)
	o.Model, o.Effort = "fx-test", "low"
	o.Instructions = Instructions{Mode: Append, Text: "be brief"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	if ref.Engine != harness.Fx || ref.ID != "fx-session-1" {
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
	if err != nil || result.Status != "completed" || result.Text != "answer" {
		t.Fatalf("%+v %v", result, err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		if e.Kind == "tool_started" && (e.Tool != "read_file" || e.ItemID != "call_1") {
			t.Errorf("tool named from its title or misidentified: %+v", e)
		}
	}
	if slices.Index(kinds, "tool_started") < 0 || slices.Index(kinds, "tool_completed") < 0 || kinds[len(kinds)-1] != "status" {
		t.Fatalf("events: %v", kinds)
	}
	final := result.Usage
	if !final.Known || !final.Final || final.Input != 85 || final.Output != 6 || final.Reasoning != 1 || !final.ReasoningKnown {
		t.Fatalf("turn accounting: %+v", final)
	}
	if !final.CacheKnown || final.CacheRead != 64 || final.CacheWrite != 0 {
		t.Fatalf("cache split: %+v", final)
	}
	c := result.Context
	if c.Quality != harness.Measured || c.UsedTokens == nil || *c.UsedTokens != 4096 || c.CapacityTokens == nil || *c.CapacityTokens != 200000 {
		t.Fatalf("context: %+v", c)
	}
	s.Close()

	logged := fxFixtureLogged(t, log)
	if logged["args"][0] != "acp" {
		t.Errorf("arguments: %q", logged["args"])
	}
	if logged["fx"][0] != "" {
		t.Errorf("an ambient fx variable reached the session: %q", logged["fx"][0])
	}
	// The mode, the model and the effort are set, and each is read back.
	want := []string{"mode=ask", "model=fx-test", "effort=low"}
	if !slices.Equal(logged["config"], want) {
		t.Errorf("configuration: %v", logged["config"])
	}
	var created struct {
		Cwd          string `json:"cwd"`
		MCP          []any  `json:"mcpServers"`
		SystemPrompt []struct{ Type, Text string }
	}
	if json.Unmarshal([]byte(logged["new"][0]), &created) != nil || created.Cwd != o.WorkDir || created.MCP == nil || len(created.MCP) != 0 {
		t.Errorf("session/new: %s", logged["new"][0])
	}
	if len(created.SystemPrompt) != 1 || created.SystemPrompt[0].Text != "be brief" {
		t.Errorf("session instructions: %s", logged["new"][0])
	}
}

func TestFxPermissionPolicyAnswersTheAgent(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   string
	}{
		{FxDenyWhenAsked, "selected:reject_once"},
		{FxAllowWhenAsked, "selected:allow_once"},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			o, log := fxFixtureOptions(t)
			o.Policy.FxPermission = tc.policy
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			turn, err := s.StartTurn(ctx, Input{Text: "write permission"})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, turn)
			if _, err = turn.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			logged := fxFixtureLogged(t, log)
			if len(logged["permission"]) != 1 || logged["permission"][0] != tc.want {
				t.Fatalf("permission answers: %v", logged["permission"])
			}
		})
	}
}

func TestFxNeverAnswersWithAnAlwaysOption(t *testing.T) {
	o, log := fxFixtureOptions(t)
	o.Policy.FxPermission = FxAllowWhenAsked
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	turn, err := s.StartTurn(ctx, Input{Text: "write permission"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	if _, err = turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	logged := fxFixtureLogged(t, log)
	if len(logged["permission"]) != 1 || strings.Contains(logged["permission"][0], "always") {
		t.Fatalf("permission answers: %v", logged["permission"])
	}
}

func TestFxResumeOfAConversationItDoesNotHoldIsRefused(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	s.Close()
	ref.ID = "fx-session-gone"
	_, err = Resume(ctx, o, ref)
	if !errors.Is(err, errConversationGone) {
		t.Fatalf("resume: %v", err)
	}
}

func TestFxAnswersARequestWithASharedOptionIdAsCancelled(t *testing.T) {
	// Two options that share one id: a deny chosen from them could be read as
	// an allow, so the request is answered cancelled instead.
	for _, policy := range []string{FxDenyWhenAsked, FxAllowWhenAsked} {
		t.Run(policy, func(t *testing.T) {
			o, log := fxFixtureOptions(t)
			o.Policy.FxPermission = policy
			t.Setenv(fxFixtureSharedOption, "1")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			turn, err := s.StartTurn(ctx, Input{Text: "write permission"})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, turn)
			if _, err = turn.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			logged := fxFixtureLogged(t, log)
			if len(logged["permission"]) != 1 || logged["permission"][0] != "cancelled:" {
				t.Fatalf("permission answers: %v", logged["permission"])
			}
		})
	}
}

func TestFxInheritsTheOperatorsProtectiveVariables(t *testing.T) {
	// The operator's own bounds, cost and network switches are inherited, and
	// accepted as additions; the rest of fx's variables stay managed.
	t.Setenv("FX_MAX_AGENT_STEPS", "20")
	t.Setenv("FX_ULTRAFAST", "0")
	t.Setenv("FX_AUTO_UPGRADE", "0")
	t.Setenv("FX_MODEL", "operator/model")
	o, log := fxFixtureOptions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	logged := fxFixtureLogged(t, log)
	if logged["fx"][0] != "0" {
		t.Fatalf("a protective fx variable did not reach the session: %v", logged["fx"])
	}
	if len(logged["fxmodel"]) != 1 || logged["fxmodel"][0] != "" {
		t.Fatalf("a managed fx variable reached the session: %v", logged["fxmodel"])
	}
}

func TestFxRefusesAManagedVariableAsAnAddition(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	o.Env = []string{"FX_MODEL=other/model"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, o)
	if err == nil || !strings.Contains(err.Error(), "FX_MODEL") {
		t.Fatalf("managed addition: %v", err)
	}
}

func TestFxAcceptsAProtectiveVariableAsAnAddition(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	o.Env = []string{"FX_MAX_AGENT_STEPS=12"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
}

func TestFxRefusesInstructionsItCannotAppend(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	o.Instructions = Instructions{Mode: Replace, Text: "be brief"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, o)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Code != RefusedNotOffered {
		t.Fatalf("replace instructions: %v", err)
	}
}

func TestFxRefusesAnotherEnginesPolicy(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	o.Policy.ClaudePermission = "dontAsk"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, o)
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("foreign policy: %v", err)
	}
}

func TestFxRefusesAModeItCannotReadBack(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	t.Setenv(fxFixtureNoMode, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, o)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Code != RefusedNotOffered {
		t.Fatalf("unreadable permission mode: %v", err)
	}
}

func TestFxRefusesAModeThatDoesNotAsk(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	t.Setenv(fxFixtureNoAsk, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, o)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Code != RefusedNotOffered {
		t.Fatalf("a mode that does not ask: %v", err)
	}
}

func TestFxRefusesAModelItDidNotTake(t *testing.T) {
	o, _ := fxFixtureOptions(t)
	o.Model = "refused-model"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, o)
	var capability *CapabilityError
	if !errors.As(err, &capability) || capability.Code != CapabilityChangedModel {
		t.Fatalf("model: %v", err)
	}
}
