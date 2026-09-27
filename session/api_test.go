package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
)

const apiToken = "sk-synthetic-SECRET-7f3a9c"

// seenRequest is what the fake endpoint read from one request.
type seenRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string            `json:"role"`
		Content    *string           `json:"content"`
		ToolCallID string            `json:"tool_call_id"`
		ToolCalls  []json.RawMessage `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	ReasoningEffort     string `json:"reasoning_effort"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	auth                string
}

func (r seenRequest) content(i int) string {
	if r.Messages[i].Content == nil {
		return ""
	}
	return *r.Messages[i].Content
}

// toolMessage is the content of the tool message answering id.
func (r seenRequest) toolMessage(id string) (string, bool) {
	for i, m := range r.Messages {
		if m.Role == "tool" && m.ToolCallID == id {
			return r.content(i), true
		}
	}
	return "", false
}

func (r seenRequest) toolNames() []string {
	var names []string
	for _, tool := range r.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

type endpointReply func(w http.ResponseWriter, r *http.Request)

// fakeEndpoint is a local Chat Completions server that answers from a script.
type fakeEndpoint struct {
	t        *testing.T
	mu       sync.Mutex
	replies  []endpointReply
	requests []seenRequest
	quit     chan struct{}
	url      string
}

func newEndpoint(t *testing.T, replies ...endpointReply) *fakeEndpoint {
	t.Helper()
	e := &fakeEndpoint{t: t, replies: replies, quit: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(func() {
		close(e.quit)
		server.Close()
	})
	e.url = server.URL + "/v1"
	return e
}

func (e *fakeEndpoint) script(replies ...endpointReply) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replies = append(e.replies, replies...)
}

func (e *fakeEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	var seen seenRequest
	if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&seen) != nil {
		e.t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	seen.auth = r.Header.Get("Authorization")
	e.mu.Lock()
	e.requests = append(e.requests, seen)
	if len(e.replies) == 0 {
		e.mu.Unlock()
		e.t.Errorf("request %d was not scripted", len(e.requests))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	reply := e.replies[0]
	e.replies = e.replies[1:]
	e.mu.Unlock()
	reply(w, r)
}

func (e *fakeEndpoint) seen() []seenRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]seenRequest(nil), e.requests...)
}

type scriptedCall struct{ id, name, args string }

// answer replies with a message and optional calls, reporting 10 prompt and 5
// completion tokens.
func answer(content string, calls ...scriptedCall) endpointReply {
	return func(w http.ResponseWriter, r *http.Request) {
		message := map[string]any{"role": "assistant", "content": content}
		finish := "stop"
		if len(calls) > 0 {
			var wire []map[string]any
			for _, c := range calls {
				wire = append(wire, map[string]any{"id": c.id, "type": "function", "function": map[string]any{"name": c.name, "arguments": c.args}})
			}
			message["tool_calls"], finish = wire, "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}
}

// hold signals arrival and then blocks until the client gives up.
func (e *fakeEndpoint) hold(arrived chan<- struct{}) endpointReply {
	return func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-r.Context().Done():
		case <-e.quit:
		}
	}
}

func status(code int, headers ...string) endpointReply {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"provider prose ` + apiToken + `"}}`))
	}
}

var readFile = ToolDefinition{Name: "read_file", Description: "Read a file.", Schema: map[string]any{"type": "object"}}

func privateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func apiOptions(t *testing.T, url string, handler ToolHandler, tools ...ToolDefinition) Options {
	t.Helper()
	if len(tools) == 0 {
		tools = []ToolDefinition{readFile}
	}
	return Options{
		Provider: harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{
			BaseURL: url, Dialect: harness.OpenAIChatCompletions,
			Credentials:     func(context.Context) (string, error) { return apiToken, nil },
			EffortParameter: harness.EffortReasoningEffort,
		}},
		Model:        "synthetic-model",
		Effort:       "high",
		RuntimeHome:  privateHome(t),
		Instructions: Instructions{Mode: Append, Text: "You are a synthetic worker."},
		Restriction:  &Restriction{Tools: ToolHost{Server: "work", Tools: tools, Handler: handler}},
	}
}

func echo(calls *[]ToolCall, mu *sync.Mutex) ToolHandler {
	return ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		mu.Lock()
		*calls = append(*calls, c)
		mu.Unlock()
		return ToolResult{Content: "contents of " + string(c.Arguments)}, nil
	})
}

func startAPI(t *testing.T, o Options) *Session {
	t.Helper()
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAPI(t, s) })
	return s
}

// closeAPI closes a session and waits for it to give up its files, which
// Windows will not remove while they are open.
func closeAPI(t *testing.T, s *Session) {
	t.Helper()
	s.Close()
	select {
	case <-s.api.released:
	case <-time.After(10 * time.Second):
		t.Error("the session did not release its state")
	}
}

type finishedTurn struct {
	result Result
	err    error
	events []Event
}

func (f finishedTurn) kinds() []string {
	var kinds []string
	for _, e := range f.events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// drainTurn collects a turn's events until it ends.
func drainTurn(t *testing.T, turn *Turn) func() finishedTurn {
	t.Helper()
	var events []Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range turn.Events() {
			events = append(events, e)
		}
	}()
	return func() finishedTurn {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := turn.Wait(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("turn did not end")
		}
		<-done
		return finishedTurn{result, err, events}
	}
}

func runAPITurnToEnd(t *testing.T, s *Session, text string) finishedTurn {
	t.Helper()
	turn, err := s.StartTurn(context.Background(), Input{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return drainTurn(t, turn)()
}

func readTranscript(t *testing.T, home, id string) ([]record, string) {
	t.Helper()
	data, err := os.ReadFile(transcriptPath(sessionDir(home, id)))
	if err != nil {
		t.Fatal(err)
	}
	var records []record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("transcript line %q: %v", line, err)
		}
		records = append(records, r)
	}
	return records, string(data)
}

func recordTypes(records []record) []string {
	var types []string
	for _, r := range records {
		types = append(types, r.Type)
	}
	return types
}

func TestAPISessionRunsToolRoundsUntilTheModelAnswers(t *testing.T) {
	e := newEndpoint(t,
		answer("", scriptedCall{"call_1", "read_file", `{"path":"a"}`}),
		answer("Looking further.", scriptedCall{"call_2", "read_file", `{"path":"b"}`}, scriptedCall{"call_3", "read_file", `{"path":"c"}`}),
		answer("All done."),
	)
	var mu sync.Mutex
	var calls []ToolCall
	o := apiOptions(t, e.url, echo(&calls, &mu))
	s := startAPI(t, o)
	ref := s.Ref()
	if ref.Engine != harness.OpenAICompatible || !validSessionID(ref.ID) || ref.Home != o.RuntimeHome || ref.WorkDir != "" || ref.ConfigHash == "" {
		t.Fatalf("ref %+v", ref)
	}
	done := runAPITurnToEnd(t, s, "Read the files.")
	if done.err != nil || done.result.Status != "completed" || done.result.Text != "All done." {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	if len(calls) != 3 || string(calls[0].Arguments) != `{"path":"a"}` || string(calls[2].Arguments) != `{"path":"c"}` || calls[0].TurnID != done.result.TurnID {
		t.Fatalf("handler calls %+v", calls)
	}
	if u := done.result.Usage; !u.Known || !u.Final || u.Input != 30 || u.Output != 15 {
		t.Fatalf("turn usage %+v", u)
	}
	// Each response's usage is published as it arrives, before its calls run.
	want := []string{"usage", "context", "tool_started", "tool_completed", "usage", "context", "text", "tool_started", "tool_completed", "tool_started", "tool_completed", "usage", "context", "text", "usage", "status"}
	got := done.kinds()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events\n got %v\nwant %v", got, want)
	}
	final := done.events[len(done.events)-2]
	if final.Usage == nil || !final.Usage.Final || final.Usage.Input != 30 {
		t.Fatalf("final usage event %+v", final)
	}
	seen := e.seen()
	if len(seen) != 3 {
		t.Fatalf("%d requests", len(seen))
	}
	first, last := seen[0], seen[2]
	if first.auth != "Bearer "+apiToken || first.Model != "synthetic-model" || first.ReasoningEffort != "high" {
		t.Fatalf("request %+v", first)
	}
	if first.Messages[0].Role != "system" || first.content(0) != "You are a synthetic worker." || first.Messages[1].Role != "user" || first.content(1) != "Read the files." {
		t.Fatalf("first request messages %+v", first.Messages)
	}
	if names := first.toolNames(); len(names) != 1 || names[0] != "read_file" {
		t.Fatalf("tools %v", names)
	}
	roles := []string{}
	for _, m := range last.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,assistant,tool,tool" {
		t.Fatalf("history %v", roles)
	}
	if text, ok := last.toolMessage("call_3"); !ok || text != `contents of {"path":"c"}` {
		t.Fatalf("tool message %q", text)
	}
	records, raw := readTranscript(t, o.RuntimeHome, ref.ID)
	if strings.Contains(raw, apiToken) || strings.Contains(raw, "SECRET") {
		t.Fatal("the credential reached the transcript")
	}
	types := strings.Join(recordTypes(records), ",")
	if types != "session,turn_start,user,usage,assistant,tool_call,tool_result,usage,assistant,tool_call,tool_result,tool_call,tool_result,usage,assistant,turn_end" {
		t.Fatalf("transcript %s", types)
	}
	if end := records[len(records)-1]; end.Status != "completed" || end.Turn != done.result.TurnID {
		t.Fatalf("turn end %+v", end)
	}
	if !s.ToolsSettled() || s.ToolsClosed() || s.Recovered().TurnID != "" {
		t.Fatal("tool state after a completed turn")
	}
	// A second turn carries the whole history, and the next input after it.
	e.script(answer("Second."))
	if done = runAPITurnToEnd(t, s, "Again."); done.result.Status != "completed" {
		t.Fatalf("%+v", done.result)
	}
	next := e.seen()[3]
	if n := len(next.Messages); next.Messages[n-2].Role != "assistant" || next.content(n-2) != "All done." || next.content(n-1) != "Again." {
		t.Fatalf("second turn history %+v", next.Messages)
	}
}

func TestAPISessionClosingToolEndsTheTurn(t *testing.T) {
	finish := ToolDefinition{Name: "finish", Description: "Report the work.", Schema: map[string]any{"type": "object"}, Closing: true}
	e := newEndpoint(t,
		// A refused finish has not finished anything, so the loop carries on.
		answer("", scriptedCall{"c1", "finish", `{"ok":false}`}),
		answer("", scriptedCall{"c2", "finish", `{"ok":true}`}, scriptedCall{"c3", "read_file", `{}`}),
	)
	var ran []string
	handler := ToolHandlerFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		ran = append(ran, c.Name+string(c.Arguments))
		if c.Name == "finish" && string(c.Arguments) == `{"ok":false}` {
			return ToolResult{Content: "not yet", IsError: true}, nil
		}
		return ToolResult{Content: "reported"}, nil
	})
	o := apiOptions(t, e.url, handler, readFile, finish)
	s := startAPI(t, o)
	done := runAPITurnToEnd(t, s, "Work, then finish.")
	if done.err != nil || done.result.Status != "completed" {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	if len(ran) != 2 || ran[1] != `finish{"ok":true}` {
		t.Fatalf("handler ran %v", ran)
	}
	if len(e.seen()) != 2 || !s.ToolsClosed() {
		t.Fatalf("the closing tool did not end the turn: %d requests, closed %v", len(e.seen()), s.ToolsClosed())
	}
	refused := false
	for _, ev := range done.events {
		if ev.Kind == "tool_refused" && ev.Tool == "read_file" && ev.Status == "channel_closed" {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("the call after the closing tool was not refused: %v", done.kinds())
	}
	records, _ := readTranscript(t, o.RuntimeHome, s.Ref().ID)
	for _, r := range records {
		if r.Type == recordToolResult && r.Call == "c3" && (r.Outcome != outcomeRefused || !r.IsError) {
			t.Fatalf("refused call recorded as %+v", r)
		}
	}
	// A later turn keeps the channel closed: every call is refused.
	e.script(answer("", scriptedCall{"c4", "read_file", `{}`}))
	done = runAPITurnToEnd(t, s, "More?")
	if done.result.Status != "completed" || len(ran) != 2 {
		t.Fatalf("%+v ran %v", done.result, ran)
	}
}

func apiSkill(t *testing.T) harness.Skill {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\ndescription: Synthetic demo skill.\n---\nSay hello politely.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "go.sh"), []byte("#!/bin/sh\necho ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return harness.Skill{Name: "demo", Dir: dir}
}

func TestAPISessionAnswersSkillCalls(t *testing.T) {
	e := newEndpoint(t,
		answer("", scriptedCall{"s1", "load_skill", `{"skill":"demo"}`}, scriptedCall{"s2", "load_skill", `{"skill":"demo","file":"../../etc/passwd"}`}),
		answer("Hello."),
	)
	var mu sync.Mutex
	var calls []ToolCall
	o := apiOptions(t, e.url, echo(&calls, &mu))
	o.Skills = harness.Skills{Provided: []harness.Skill{apiSkill(t)}}
	s := startAPI(t, o)
	done := runAPITurnToEnd(t, s, "Greet me.")
	if done.err != nil || done.result.Text != "Hello." || len(calls) != 0 {
		t.Fatalf("%+v %v handler %v", done.result, done.err, calls)
	}
	first, second := e.seen()[0], e.seen()[1]
	if names := strings.Join(first.toolNames(), ","); names != "read_file,load_skill" {
		t.Fatalf("tools %s", names)
	}
	if system := first.content(0); !strings.HasPrefix(system, "You are a synthetic worker.\n\n") || !strings.Contains(system, "demo: Synthetic demo skill.") {
		t.Fatalf("system message %q", system)
	}
	if text, _ := second.toolMessage("s1"); !strings.Contains(text, "Say hello politely.") {
		t.Fatalf("skill answer %q", text)
	}
	if text, _ := second.toolMessage("s2"); !strings.HasPrefix(text, "load_skill error: ") {
		t.Fatalf("escaping read answered %q", text)
	}
	statuses := []string{}
	for _, ev := range done.events {
		if ev.Kind == "tool_completed" {
			statuses = append(statuses, ev.Tool+":"+ev.Status)
		}
	}
	if strings.Join(statuses, ",") != "load_skill:completed,load_skill:failed" {
		t.Fatalf("skill events %v", statuses)
	}
}

func TestAPISessionRunsSkillScriptsAsSkillRunSays(t *testing.T) {
	skill := apiSkill(t)
	skill.Scripts = true
	e := newEndpoint(t, answer("", scriptedCall{"r1", "run_skill_script", `{"skill":"demo","script":"scripts/go.sh","args":["x"]}`}), answer("Ran."))
	var mu sync.Mutex
	var calls []ToolCall
	o := apiOptions(t, e.url, echo(&calls, &mu))
	o.Skills = harness.Skills{Provided: []harness.Skill{skill}}
	if _, err := Start(context.Background(), o); err == nil {
		t.Fatal("scripts offered without SkillRun.WorkDir")
	}
	var ran []SkillCommand
	o.SkillRun = SkillRunOptions{WorkDir: t.TempDir(), Exec: func(_ context.Context, c SkillCommand) (SkillOutput, error) {
		ran = append(ran, c)
		return SkillOutput{Stdout: "exec ran"}, nil
	}}
	s := startAPI(t, o)
	done := runAPITurnToEnd(t, s, "Run it.")
	if done.err != nil || len(ran) != 1 || strings.Join(ran[0].Args, ",") != "x" {
		t.Fatalf("%v ran %+v", done.err, ran)
	}
	if text, _ := e.seen()[1].toolMessage("r1"); !strings.Contains(text, "exec ran") {
		t.Fatalf("script answer %q", text)
	}
}

func TestAPISessionBoundsModelCallsPerTurn(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var calls []ToolCall
	o := apiOptions(t, "https://gateway.invalid/v1", echo(&calls, &mu))
	o.Loop.MaxSteps = 3
	o.complete = func(_ context.Context, cfg completion.Config, messages []completion.Message, tools []completion.Tool) (completion.Result, error) {
		n := requests.Add(1)
		if cfg.Model != "synthetic-model" || cfg.MaxContextBytes != defaultLoopRequestBytes || len(tools) != 1 || messages[0].Role != "system" {
			t.Errorf("config %+v tools %v", cfg, tools)
		}
		call := completion.ToolCall{ID: "loop", Type: "function"}
		call.Function.Name, call.Function.Arguments = "read_file", `{}`
		result := completion.Result{Message: completion.Message{Role: "assistant", ToolCalls: []completion.ToolCall{call}}}
		if n != 2 {
			result.Usage = harness.Usage{Known: true, Input: 4, Output: 1}
		}
		return result, nil
	}
	s := startAPI(t, o)
	done := runAPITurnToEnd(t, s, "Loop forever.")
	var failure *TurnError
	if done.result.Status != "failed" || !errors.As(done.err, &failure) || failure.Code != TurnStepLimit || !errors.Is(done.err, ErrTurnFailed) {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	if requests.Load() != 3 || len(calls) != 3 {
		t.Fatalf("%d requests, %d calls", requests.Load(), len(calls))
	}
	if done.result.Usage.Known || done.result.NativeError {
		t.Fatalf("a turn with an unreported response claims known usage: %+v", done.result)
	}
	if !done.result.Observed.Known || done.result.Observed.Input != 8 {
		t.Fatalf("observed %+v", done.result.Observed)
	}
	facts, ok := harness.ErrorFacts(done.err)
	if !ok || facts.Family != harness.FailureTurn || facts.Code != TurnStepLimit || facts.Retryable || facts.Engine != harness.OpenAICompatible {
		t.Fatalf("facts %+v", facts)
	}
	// The session stays usable after a failed turn.
	if s.Health().State != Idle {
		t.Fatalf("health %+v", s.Health())
	}
}

func TestAPISessionFailedRequestKeepsItsFacts(t *testing.T) {
	e := newEndpoint(t, status(http.StatusTooManyRequests, "Retry-After", "7"), answer("Recovered."))
	o := apiOptions(t, e.url, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }))
	s := startAPI(t, o)
	done := runAPITurnToEnd(t, s, "Try.")
	facts, ok := harness.ErrorFacts(done.err)
	if done.result.Status != "failed" || !ok || facts.Family != harness.FailureTurn || facts.Cause != harness.CauseRateLimited || facts.RetryAfter != 7*time.Second || facts.Retryable || facts.Operation != harness.Session {
		t.Fatalf("%+v %+v %v", done.result, facts, done.err)
	}
	if !errors.Is(done.err, ErrTurnFailed) || strings.Contains(done.err.Error(), "prose") || !done.result.NativeError {
		t.Fatalf("error %v", done.err)
	}
	if done = runAPITurnToEnd(t, s, "Again."); done.result.Status != "completed" {
		t.Fatalf("the session did not survive a failed request: %+v %v", done.result, done.err)
	}
	_, raw := readTranscript(t, o.RuntimeHome, s.Ref().ID)
	if strings.Contains(raw, apiToken) || strings.Contains(raw, "prose") {
		t.Fatal("provider text or the credential reached the transcript")
	}
}

func TestAPISessionInterruptCancelsTheRequest(t *testing.T) {
	e := newEndpoint(t)
	arrived := make(chan struct{})
	e.script(e.hold(arrived))
	o := apiOptions(t, e.url, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }))
	s := startAPI(t, o)
	turn, err := s.StartTurn(context.Background(), Input{Text: "Think hard."})
	if err != nil {
		t.Fatal(err)
	}
	finished := drainTurn(t, turn)
	<-arrived
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = s.Interrupt(ctx, turn.ID()); err != nil {
		t.Fatal(err)
	}
	done := finished()
	if done.result.Status != "interrupted" || done.err != nil || done.result.Usage.Known {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	if c := s.Capabilities().Interrupt; c.Availability != harness.Composed {
		t.Fatalf("interrupt capability %+v", c)
	}
	records, _ := readTranscript(t, o.RuntimeHome, s.Ref().ID)
	if end := records[len(records)-1]; end.Type != recordTurnEnd || end.Status != "interrupted" || end.Code != "" {
		t.Fatalf("turn end %+v", end)
	}
	e.script(answer("Fresh."))
	done = runAPITurnToEnd(t, s, "Next.")
	history := e.seen()[1]
	if done.result.Status != "completed" || len(history.Messages) != 3 || history.content(1) != "Think hard." || history.content(2) != "Next." {
		t.Fatalf("%+v history %+v", done.result, history.Messages)
	}
}

func TestAPISessionInterruptCancelsTheRunningHandler(t *testing.T) {
	e := newEndpoint(t, answer("", scriptedCall{"w1", "write_file", `{}`}, scriptedCall{"w2", "write_file", `{}`}))
	entered := make(chan struct{})
	release := make(chan struct{})
	var cancelled atomic.Bool
	var runs atomic.Int32
	handler := ToolHandlerFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		runs.Add(1)
		close(entered)
		<-ctx.Done()
		cancelled.Store(true)
		// It takes a moment to stop, as a real write would.
		<-release
		return ToolResult{Content: "stopped part-way"}, nil
	})
	o := apiOptions(t, e.url, handler, ToolDefinition{Name: "write_file", Schema: map[string]any{"type": "object"}})
	s := startAPI(t, o)
	turn, err := s.StartTurn(context.Background(), Input{Text: "Write."})
	if err != nil {
		t.Fatal(err)
	}
	finished := drainTurn(t, turn)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = s.Interrupt(ctx, turn.ID()); err != nil {
		t.Fatal(err)
	}
	if done := finished(); done.result.Status != "interrupted" {
		t.Fatalf("%+v", done.result)
	}
	if !cancelled.Load() || s.ToolsSettled() {
		t.Fatal("the running handler was not cancelled, or was reported settled while it ran")
	}
	if _, err = s.StartTurn(ctx, Input{Text: "Next."}); !errors.Is(err, ErrToolsUnsettled) {
		t.Fatalf("a turn started over an unsettled call: %v", err)
	}
	close(release)
	if err = s.AwaitToolsSettled(ctx); err != nil {
		t.Fatal(err)
	}
	e.script(answer("Checked."))
	if done := runAPITurnToEnd(t, s, "Check what happened."); done.result.Status != "completed" {
		t.Fatalf("%+v", done.result)
	}
	history := e.seen()[1]
	if text, _ := history.toolMessage("w1"); text != "stopped part-way" {
		t.Fatalf("the late result was not recorded: %q", text)
	}
	if text, _ := history.toolMessage("w2"); text != notRunText {
		t.Fatalf("the unreached call was answered %q", text)
	}
	if runs.Load() != 1 {
		t.Fatalf("handler ran %d times", runs.Load())
	}
}

func TestAPISessionSteerIsComposed(t *testing.T) {
	e := newEndpoint(t)
	arrived := make(chan struct{})
	e.script(e.hold(arrived), answer("Steered."))
	o := apiOptions(t, e.url, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }))
	s := startAPI(t, o)
	turn, err := s.StartTurn(context.Background(), Input{Text: "Go left."})
	if err != nil {
		t.Fatal(err)
	}
	finished := drainTurn(t, turn)
	<-arrived
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var unsupported *UnsupportedError
	if _, err = s.Steer(ctx, turn.ID(), Input{Text: "Go right."}, SteerOptions{RequireNative: true}); !errors.As(err, &unsupported) || unsupported.Capability.Availability != harness.Composed {
		t.Fatalf("native steering claimed: %v", err)
	}
	steered, err := s.Steer(ctx, turn.ID(), Input{Text: "Go right."}, SteerOptions{})
	if err != nil || steered.Strategy != harness.Composed {
		t.Fatalf("%+v %v", steered, err)
	}
	if done := finished(); done.result.Status != "interrupted" {
		t.Fatalf("%+v", done.result)
	}
	if done := drainTurn(t, steered.Turn)(); done.result.Text != "Steered." {
		t.Fatalf("%+v", done.result)
	}
}

// A session that stopped mid-call leaves a call recorded without a result.
// Resuming answers it as an unknown outcome and never runs it again.
func TestAPISessionResumeAfterACrashNeverRerunsACall(t *testing.T) {
	e := newEndpoint(t, answer("", scriptedCall{"w1", "write_file", `{"path":"x"}`}))
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var first atomic.Int32
	blocking := ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		first.Add(1)
		close(entered)
		<-release
		return ToolResult{Content: "written"}, nil
	})
	write := ToolDefinition{Name: "write_file", Schema: map[string]any{"type": "object"}}
	o := apiOptions(t, e.url, blocking, write)
	s := startAPI(t, o)
	turn, err := s.StartTurn(context.Background(), Input{Text: "Write x."})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range turn.Events() {
		}
	}()
	<-entered
	ref := s.Ref()
	// What a crash leaves on disk: the transcript as it stands, plus a record
	// cut off mid-write.
	snapshot, err := os.ReadFile(transcriptPath(sessionDir(o.RuntimeHome, ref.ID)))
	if err != nil {
		t.Fatal(err)
	}
	crashed := privateHome(t)
	dir := sessionDir(crashed, ref.ID)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(transcriptPath(dir), append(snapshot, []byte(`{"type":"tool_res`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	var second atomic.Int32
	o2 := o
	o2.RuntimeHome = crashed
	o2.Restriction = &Restriction{Tools: ToolHost{Server: "work", Tools: []ToolDefinition{write}, Handler: ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		second.Add(1)
		return ToolResult{Content: "again"}, nil
	})}}
	ref2 := ref
	ref2.Home = crashed
	resumed, err := Resume(context.Background(), o2, ref2)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI(t, resumed)
	recovery := resumed.Recovered()
	if recovery.TurnID != turn.ID() || len(recovery.UnknownOutcomes) != 1 || recovery.UnknownOutcomes[0] != (RecoveredCall{ID: "w1", Tool: "write_file"}) {
		t.Fatalf("recovery %+v", recovery)
	}
	e.script(answer("Checked."))
	done := runAPITurnToEnd(t, resumed, "Did it work?")
	if done.result.Status != "completed" || second.Load() != 0 || first.Load() != 1 {
		t.Fatalf("%+v: first handler %d, resumed handler %d", done.result, first.Load(), second.Load())
	}
	history := e.seen()[1]
	if text, ok := history.toolMessage("w1"); !ok || text != unknownOutcomeText {
		t.Fatalf("dangling call answered %q", text)
	}
	records, _ := readTranscript(t, crashed, ref.ID)
	types := strings.Join(recordTypes(records), ",")
	if types != "session,turn_start,user,usage,assistant,tool_call,tool_result,turn_end,turn_start,user,usage,assistant,turn_end" {
		t.Fatalf("transcript %s", types)
	}
	if r := records[6]; r.Outcome != outcomeUnknown || !r.IsError || records[7].Status != "interrupted" || records[7].Code != "session_interrupted" {
		t.Fatalf("recovery records %+v %+v", r, records[7])
	}
	// Recovery is recorded, so resuming again finds nothing new.
	resumed.Close()
	if _, err = resumed.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, err := Resume(context.Background(), o2, ref2)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI(t, again)
	if again.Recovered().TurnID != "" || len(again.Recovered().UnknownOutcomes) != 0 {
		t.Fatalf("recovery repeated: %+v", again.Recovered())
	}
}

// Closing a session whose handler is still running keeps the conversation
// locked until the handler returns, so no other session answers that call as
// unknown while it may still be taking effect; its result is then recorded.
func TestAPISessionLockIsHeldUntilCallsSettle(t *testing.T) {
	e := newEndpoint(t, answer("", scriptedCall{"w1", "write_file", `{}`}))
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) {
		close(entered)
		<-release
		return ToolResult{Content: "written"}, nil
	})
	o := apiOptions(t, e.url, handler, ToolDefinition{Name: "write_file", Schema: map[string]any{"type": "object"}})
	s := startAPI(t, o)
	ref := s.Ref()
	if _, err := Resume(context.Background(), o, ref); err == nil {
		t.Fatal("a second session opened a locked conversation")
	} else if facts, ok := harness.ErrorFacts(err); !errors.Is(err, ErrLeaseHeld) || !ok || facts.Code != StateLocked {
		t.Fatalf("lock contention %v %+v", err, facts)
	}
	if _, _, err := Open(context.Background(), o, &ref); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("Open started fresh over a locked conversation: %v", err)
	}
	turn, err := s.StartTurn(context.Background(), Input{Text: "Write."})
	if err != nil {
		t.Fatal(err)
	}
	finished := drainTurn(t, turn)
	<-entered
	s.Close()
	if done := finished(); done.result.Status != "failed" || !errors.Is(done.err, ErrClosed) {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	if _, err = Resume(context.Background(), o, ref); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("the lock was given up while a call ran: %v", err)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if out, err := s.Release(short); !errors.Is(err, ErrUnreclaimed) || out.Confirmed {
		t.Fatalf("release confirmed with a call running: %+v %v", out, err)
	}
	close(release)
	if out, err := s.Release(context.Background()); err != nil || !out.Confirmed {
		t.Fatalf("%+v %v", out, err)
	}
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI(t, resumed)
	if r := resumed.Recovered(); r.TurnID != "" || len(r.UnknownOutcomes) != 0 {
		t.Fatalf("a settled call was reported unknown: %+v", r)
	}
	e.script(answer("Done."))
	runAPITurnToEnd(t, resumed, "Status?")
	if text, _ := e.seen()[1].toolMessage("w1"); text != "written" {
		t.Fatalf("the late result was lost: %q", text)
	}
}

func TestAPISessionOpenFallsBackAndRefusesMismatches(t *testing.T) {
	e := newEndpoint(t)
	o := apiOptions(t, e.url, ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }))
	s, opened, err := Open(context.Background(), o, nil)
	if err != nil || opened != (Opened{}) {
		t.Fatal(opened, err)
	}
	ref := s.Ref()
	if _, err = s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, opened, err := Open(context.Background(), o, &ref)
	if err != nil || !opened.Resumed || resumed.Ref() != ref {
		t.Fatal(opened, err)
	}
	closeAPI(t, resumed)

	changed := o
	changed.Model = "another-model"
	if _, err = Resume(context.Background(), changed, ref); !errors.Is(err, ErrIncompatibleResume) {
		t.Fatalf("a changed model resumed: %v", err)
	}
	fresh, opened, err := Open(context.Background(), changed, &ref)
	if err != nil || opened.Fresh != FreshIncompatible || fresh.Ref().ID == ref.ID {
		t.Fatal(opened, err)
	}
	closeAPI(t, fresh)

	escaping := ref
	escaping.ID = "../escape"
	if _, err = Resume(context.Background(), o, escaping); !errors.Is(err, ErrIncompatibleResume) {
		t.Fatalf("a path-shaped session ID was used: %v", err)
	}
	missing := ref
	missing.ID = newID()
	var state *StateError
	if _, err = Resume(context.Background(), o, missing); !errors.As(err, &state) || state.Code != StateMissing {
		t.Fatalf("missing conversation: %v", err)
	}
	fresh, opened, err = Open(context.Background(), o, &missing)
	if err != nil || opened.Fresh != FreshUnavailable {
		t.Fatal(opened, err)
	}
	closeAPI(t, fresh)

	// A transcript that names another configuration is not resumed.
	forged := ref
	forged.ConfigHash = apiReference(func() Options { n, _ := normalize(changed); return n }(), ref.ID).ConfigHash
	if _, err = Resume(context.Background(), changed, forged); !errors.As(err, &state) || state.Code != StateCorrupt {
		t.Fatalf("a transcript for another configuration resumed: %v", err)
	}
}

// Callers persist Refs. This literal pins an OpenAI-compatible session's
// digest: the endpoint's base URL and dialect, model, effort, instructions,
// tool server and skills, never a credential.
func TestAPIRefIsStableAndCarriesNoCredential(t *testing.T) {
	o := Options{
		Provider: harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{BaseURL: "https://gateway.invalid/v1", Dialect: harness.OpenAIChatCompletions,
			Credentials: func(context.Context) (string, error) { return apiToken, nil }, EffortParameter: harness.EffortReasoningObject}},
		Model: "xai/grok-4", Effort: "low", RuntimeHome: "/state", AccountIdentity: "team",
		Instructions: Instructions{Mode: Replace, Text: "be brief"},
		Restriction:  &Restriction{Tools: ToolHost{Server: "work", Tools: []ToolDefinition{readFile}}},
	}
	got, _ := json.Marshal(apiReference(o, "s1"))
	const want = `{"engine":"openai-compatible","id":"s1","home":"/state","work_dir":"","account_identity":"team","config_hash":"9d56d6c21daf25cac0a0aa0e9c0530d898b003ce4ad426d087c5137b6654e860"}`
	if string(got) != want {
		t.Fatalf("persisted ref changed:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(string(got), apiToken) {
		t.Fatal("credential in ref")
	}
	same := o
	same.Provider.API.Credentials = func(context.Context) (string, error) { return "other", nil }
	same.Restriction = &Restriction{Tools: ToolHost{Server: "work", Tools: []ToolDefinition{readFile, {Name: "added", Schema: map[string]any{}}}}}
	if apiReference(same, "s1") != apiReference(o, "s1") {
		t.Fatal("the credential source or the tool list moved the digest")
	}
	for name, edit := range map[string]func(*Options){
		"base url":     func(o *Options) { o.Provider.API.BaseURL = "https://other.invalid/v1" },
		"model":        func(o *Options) { o.Model = "x" },
		"effort":       func(o *Options) { o.Effort = "high" },
		"instructions": func(o *Options) { o.Instructions.Text = "be long" },
		"server":       func(o *Options) { o.Restriction = &Restriction{Tools: ToolHost{Server: "other"}} },
		"skills":       func(o *Options) { o.Skills.Global = harness.GlobalSkillsExclude },
	} {
		changed := o
		edit(&changed)
		if apiReference(changed, "s1").ConfigHash == apiReference(o, "s1").ConfigHash {
			t.Errorf("%s did not move the digest", name)
		}
	}
}

func TestAPISessionRefusesWhatItCannotHonour(t *testing.T) {
	handler := ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil })
	for _, tc := range []struct {
		name   string
		edit   func(*Options)
		code   string
		family harness.Family
	}{
		{"sandbox", func(o *Options) { o.Sandbox = &Sandbox{} }, RefusedNotOffered, harness.FailureCapability},
		{"model", func(o *Options) { o.Model = "" }, RefusedModelRequired, harness.FailureCapability},
		{"work dir", func(o *Options) { o.WorkDir = "/w" }, RefusedConflict, harness.FailureCapability},
		{"env", func(o *Options) { o.Env = []string{"A=b"} }, RefusedConflict, harness.FailureCapability},
		{"policy", func(o *Options) { o.Policy.ClaudeTools = []string{} }, RefusedOtherEnginePolicy, harness.FailureCapability},
		{"effort without parameter", func(o *Options) { o.Provider.API.EffortParameter = "" }, "api_effort_parameter_required", harness.FailurePreflight},
		{"no restriction", func(o *Options) { o.Restriction = nil }, RefusedNotConfigured, harness.FailurePreflight},
		{"bridge", func(o *Options) { o.Restriction.Tools.Bridge = Bridge{Path: "/bin/bridge"} }, RefusedConflict, harness.FailureCapability},
		{"tool dir", func(o *Options) { o.Restriction.Tools.Dir = "/tmp" }, RefusedConflict, harness.FailureCapability},
		{"probe", func(o *Options) { o.Restriction.Probe = time.Second }, RefusedConflict, harness.FailureCapability},
		{"no handler", func(o *Options) { o.Restriction.Tools.Handler = nil }, RefusedToolHost, harness.FailurePreflight},
		{"no runtime home", func(o *Options) { o.RuntimeHome = "" }, RefusedRuntimeHome, harness.FailurePreflight},
		{"missing runtime home", func(o *Options) { o.RuntimeHome = filepath.Join(o.RuntimeHome, "absent") }, RefusedRuntimeHome, harness.FailurePreflight},
		{"loop bound", func(o *Options) { o.Loop.MaxSteps = 5000 }, RefusedLimit, harness.FailurePreflight},
		{"global skills", func(o *Options) { o.Skills.Global = harness.GlobalSkillsInclude }, RefusedGlobalSkills, harness.FailurePreflight},
		{"skill run without skills", func(o *Options) { o.SkillRun.WorkDir = "/w" }, RefusedConflict, harness.FailureCapability},
		{"cli half", func(o *Options) { o.Provider.CLI.Home = "/h" }, "cli_config_for_api_engine", harness.FailurePreflight},
		{"reserved tool name", func(o *Options) {
			o.Skills.Provided = []harness.Skill{apiSkill(t)}
			o.Restriction.Tools.Tools = []ToolDefinition{{Name: "load_skill", Schema: map[string]any{}}}
		}, RefusedSkillToolReserved, harness.FailurePreflight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := apiOptions(t, "https://gateway.invalid/v1", handler)
			tc.edit(&o)
			s, err := Start(context.Background(), o)
			if s != nil {
				s.Close()
				t.Fatal("started")
			}
			facts, ok := harness.ErrorFacts(err)
			if !ok || facts.Code != tc.code || facts.Family != tc.family || facts.Engine != harness.OpenAICompatible {
				t.Fatalf("facts %+v (%v)", facts, err)
			}
		})
	}
	if runtime.GOOS != "windows" {
		o := apiOptions(t, "https://gateway.invalid/v1", handler)
		if err := os.Chmod(o.RuntimeHome, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Start(context.Background(), o); err == nil {
			t.Fatal("a runtime home others can read was accepted")
		}
	}
	codex := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir(), Loop: Loop{MaxSteps: 3}}
	if _, err := normalize(codex); err == nil {
		t.Fatal("a CLI session accepted Loop")
	}
}

func TestAPISessionTelemetryAndControlsItDoesNotOffer(t *testing.T) {
	o := apiOptions(t, "https://gateway.invalid/v1", ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }))
	s := startAPI(t, o)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"compact": func() error { _, err := s.Compact(ctx); return err },
		"account": func() error { _, err := s.ReadAccount(ctx); return err },
		"quota":   func() error { _, err := s.ReadQuota(ctx); return err },
		"inspect": func() error { _, err := Inspect(ctx, o); return err },
	} {
		var unsupported *UnsupportedError
		if err := call(); !errors.As(err, &unsupported) || unsupported.Capability.Usable() || unsupported.Capability.Reason == "" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c, err := s.ReadContext(ctx); err != nil || c.UsedTokens != nil {
		t.Fatalf("context %+v %v", c, err)
	}
	if err := VerifyRestriction(ctx, o); err != nil {
		t.Fatalf("an API session's restriction holds by construction: %v", err)
	}
	caps := s.Capabilities()
	if caps.Start.Availability != harness.Composed || caps.Resume.Availability != harness.Composed || caps.RestrictTools.Availability != harness.Composed || caps.Compact.Usable() || caps.Quota.Usable() || caps.Account.Usable() || caps.Context.Availability != harness.Unknown {
		t.Fatalf("capabilities %+v", caps)
	}
}

// A reply cap reaches every model request of the turn, and a negative one is
// refused before anything starts.
func TestAPISessionCapsEveryResponse(t *testing.T) {
	e := newEndpoint(t, answer("", scriptedCall{"call_1", "read_file", `{"path":"a"}`}), answer("Done."))
	var mu sync.Mutex
	var calls []ToolCall
	o := apiOptions(t, e.url, echo(&calls, &mu))
	o.Loop.MaxOutputTokens = 256
	s := startAPI(t, o)
	if done := runAPITurnToEnd(t, s, "Read it."); done.err != nil {
		t.Fatal(done.err)
	}
	seen := e.seen()
	if len(seen) != 2 || seen[0].MaxCompletionTokens != 256 || seen[1].MaxCompletionTokens != 256 {
		t.Fatalf("requests %+v", seen)
	}
	bad := apiOptions(t, e.url, echo(&calls, &mu))
	bad.Loop.MaxOutputTokens = -1
	if _, err := Start(context.Background(), bad); err == nil {
		t.Fatal("a negative reply cap was accepted")
	}
}
