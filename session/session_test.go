package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

type fakeWire struct {
	mu        sync.Mutex
	calls     []string
	requestFn func(string, map[string]any) (json.RawMessage, error)
	sendFn    func(map[string]any) error
	closed    bool
}

func (w *fakeWire) send(ctx context.Context, m map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	kind, _ := m["type"].(string)
	w.mu.Lock()
	w.calls = append(w.calls, "send:"+kind)
	w.mu.Unlock()
	if w.sendFn != nil {
		return w.sendFn(m)
	}
	return nil
}
func (w *fakeWire) request(ctx context.Context, method string, p map[string]any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.calls = append(w.calls, method)
	w.mu.Unlock()
	if w.requestFn != nil {
		return w.requestFn(method, p)
	}
	return json.RawMessage(`{}`), nil
}
func (w *fakeWire) close() { w.mu.Lock(); w.closed = true; w.mu.Unlock() }
func fakeSession(t *testing.T, e harness.Engine) (*Session, *fakeWire) {
	t.Helper()
	o, err := normalize(Options{Provider: harness.Provider{Engine: e, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWire{}
	s := &Session{options: o, ref: reference(o, "session-1"), caps: CapabilitiesFor(e), transport: w, done: make(chan struct{}), opGate: make(chan struct{}, 1)}
	t.Cleanup(s.Close)
	return s, w
}
func notify(s *Session, text string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		panic(err)
	}
	s.notification(m)
}
func testContext(t *testing.T) context.Context {
	t.Helper()
	// This bounds hung fixtures, rather than asserting startup performance.
	// A scenario may launch several race-instrumented children sequentially.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func finishClaude(s *Session, failed bool) {
	if failed {
		notify(s, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"session-1","errors":["secret provider error"]}`)
	} else {
		notify(s, `{"type":"result","subtype":"success","is_error":false,"result":"answer","session_id":"session-1","usage":{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":40}}`)
	}
}

func TestCodexBuffersEventsBeforeStartResponse(t *testing.T) {
	s, w := fakeSession(t, harness.Codex)
	ctx := testContext(t)
	w.requestFn = func(method string, p map[string]any) (json.RawMessage, error) {
		if method != "turn/start" {
			t.Fatalf("unexpected %s", method)
		}
		notify(s, `{"method":"turn/started","params":{"threadId":"session-1","turn":{"id":"turn-1"}}}`)
		notify(s, `{"method":"item/agentMessage/delta","params":{"threadId":"session-1","turnId":"turn-1","itemId":"msg","delta":"hello"}}`)
		notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
		return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
	}
	turn, err := s.StartTurn(ctx, Input{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello" || result.TurnID != "turn-1" {
		t.Fatalf("%+v", result)
	}
	for event := range turn.Events() {
		if event.TurnID != "turn-1" {
			t.Fatalf("unbound event: %+v", event)
		}
	}
}
func TestClaudeComposedSteerWaitsForTerminal(t *testing.T) {
	s, w := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"initial"})
	if err != nil {
		t.Fatal(err)
	}
	ack := make(chan struct{})
	w.requestFn = func(method string, _ map[string]any) (json.RawMessage, error) {
		if method != "interrupt" {
			t.Errorf("unexpected %s", method)
		}
		close(ack)
		return json.RawMessage(`{}`), nil
	}
	completed := make(chan SteerResult, 1)
	errs := make(chan error, 1)
	go func() { r, e := s.Steer(ctx, turn.ID(), Input{"redirect"}, SteerOptions{}); completed <- r; errs <- e }()
	<-ack
	select {
	case <-completed:
		t.Fatal("continued before terminal result")
	case <-time.After(10 * time.Millisecond):
	}
	finishClaude(s, true)
	next := <-completed
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if next.Strategy != harness.Composed || next.Turn == turn || next.Turn.ID() == turn.ID() {
		t.Fatalf("%+v", next)
	}
	old, err := turn.Wait(ctx)
	if err != nil || old.Status != "interrupted" {
		t.Fatalf("%+v %v", old, err)
	}
	finishClaude(s, false)
	result, err := next.Turn.Wait(ctx)
	if err != nil || result.Text != "answer" || result.Usage.CacheRead != 40 {
		t.Fatalf("%+v %v", result, err)
	}
	if s.Capabilities().Steer.Availability != harness.Composed {
		t.Fatal("unrecorded capability")
	}
	w.mu.Lock()
	calls := append([]string{}, w.calls...)
	w.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"send:user", "interrupt", "send:user"}) {
		t.Fatal(calls)
	}
}
func TestSteerRequiresNativeAndExpectedTurn(t *testing.T) {
	s, w := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"initial"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Steer(ctx, "stale", Input{"redirect"}, SteerOptions{})
	if !errors.Is(err, ErrStaleTurn) {
		t.Fatal(err)
	}
	_, err = s.Steer(ctx, turn.ID(), Input{"redirect"}, SteerOptions{RequireNative: true})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	w.mu.Lock()
	n := len(w.calls)
	w.mu.Unlock()
	if n != 1 {
		t.Fatal("rejected steering wrote to CLI")
	}
	if _, err = s.StartTurn(ctx, Input{"second"}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
func TestCodexSteerPreservesTurnAndUnsupportedCapability(t *testing.T) {
	s, w := fakeSession(t, harness.Codex)
	ctx := testContext(t)
	w.requestFn = func(method string, p map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
	}
	turn, err := s.StartTurn(ctx, Input{"initial"})
	if err != nil {
		t.Fatal(err)
	}
	w.requestFn = func(method string, p map[string]any) (json.RawMessage, error) {
		if method != "turn/steer" || p["expectedTurnId"] != "turn-1" {
			t.Fatalf("%s %+v", method, p)
		}
		return json.RawMessage(`{"turnId":"turn-1"}`), nil
	}
	r, err := s.Steer(ctx, turn.ID(), Input{"new"}, SteerOptions{RequireNative: true})
	if err != nil || r.Turn != turn || r.Strategy != harness.Native {
		t.Fatalf("%+v %v", r, err)
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) { return nil, ErrUnsupported }
	_, err = s.Steer(ctx, turn.ID(), Input{"new"}, SteerOptions{})
	if !errors.Is(err, ErrUnsupported) || s.Capabilities().Steer.Availability != harness.Unsupported {
		t.Fatal(err)
	}
}
func TestUncertainControlClosesSession(t *testing.T) {
	for _, operation := range []string{"steer", "interrupt"} {
		t.Run(operation, func(t *testing.T) {
			s, w := fakeSession(t, harness.Codex)
			ctx := testContext(t)
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
			}
			turn, err := s.StartTurn(ctx, Input{"initial"})
			if err != nil {
				t.Fatal(err)
			}
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) { return nil, context.DeadlineExceeded }
			if operation == "steer" {
				_, err = s.Steer(ctx, turn.ID(), Input{"new"}, SteerOptions{})
			} else {
				err = s.Interrupt(ctx, turn.ID())
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if _, err = s.StartTurn(ctx, Input{"unsafe retry"}); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
		})
	}
}
func TestBackpressureStopsInsteadOfDropping(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	s.options.EventBuffer = 1
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","message":{"id":"a","content":[{"type":"tool_use","id":"1","name":"Read"},{"type":"tool_use","id":"2","name":"Write"}]}}`)
	_, err = turn.Wait(ctx)
	if !errors.Is(err, ErrBackpressure) {
		t.Fatal(err)
	}
}
func TestTurnCancellationAndConcurrentClose(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = turn.Wait(testContext(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Close(); _ = s.Ref(); _ = s.Capabilities() }()
	}
	wg.Wait()
}
func TestFailedResultOmitsProviderError(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	finishClaude(s, true)
	r, err := turn.Wait(ctx)
	if !errors.Is(err, ErrTurnFailed) || strings.Contains(r.Text, "secret") {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestResumeIdentityAndNativeEnvironment(t *testing.T) {
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir(), AccountIdentity: "personal", Instructions: Instructions{Append, "private instructions"}, Policy: Policy{ClaudeTools: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	r := reference(o, "id")
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "private instructions") {
		t.Fatal("instructions leaked")
	}
	if !compatible(o, r) {
		t.Fatal("same options incompatible")
	}
	changed := o
	changed.Policy.ClaudeTools = nil
	if compatible(changed, r) {
		t.Fatal("nil tools widened resume")
	}
	changed = o
	changed.Provider.CLI.Home = t.TempDir()
	if compatible(changed, r) {
		t.Fatal("different home accepted")
	}
	changed = o
	changed.AccountIdentity = "work"
	if compatible(changed, r) {
		t.Fatal("different account accepted")
	}
	t.Setenv("USER", "native-login-user")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	env := strings.Join(environment(o), "\n")
	if strings.Contains(env, "must-not-leak") || !strings.Contains(env, "USER=native-login-user") {
		t.Fatal("bad environment filtering")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	o.Provider.CLI.Home = home + "/.claude"
	if strings.Contains(strings.Join(environment(o), "\n"), "CLAUDE_CONFIG_DIR=") {
		t.Fatal("default keychain namespace changed")
	}
}
func TestCodexUsageDoesNotChargeResumedHistory(t *testing.T) {
	s, w := fakeSession(t, harness.Codex)
	ctx := testContext(t)
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
	}
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	event := `{"method":"thread/tokenUsage/updated","params":{"threadId":"session-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":100,"cachedInputTokens":80,"outputTokens":10,"reasoningOutputTokens":0},"total":{"inputTokens":90000,"cachedInputTokens":60000,"outputTokens":3000,"reasoningOutputTokens":0}}}}`
	notify(s, event)
	notify(s, event)
	notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
	r, err := turn.Wait(ctx)
	// Codex's inputTokens already counts cached input, as the shared Input does.
	if err != nil || r.Usage.Input != 100 || r.Usage.CacheRead != 80 || !r.Usage.CacheKnown || r.Usage.Output != 10 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestMissingUsageRemainsUnknown(t *testing.T) {
	for _, e := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(e), func(t *testing.T) {
			s, w := fakeSession(t, e)
			ctx := testContext(t)
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
			}
			turn, err := s.StartTurn(ctx, Input{"start"})
			if err != nil {
				t.Fatal(err)
			}
			if e == harness.Codex {
				notify(s, `{"method":"thread/tokenUsage/updated","params":{"threadId":"session-1","turnId":"turn-1","tokenUsage":{}}}`)
				notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
			} else {
				notify(s, `{"type":"result","subtype":"success","usage":{}}`)
			}
			r, err := turn.Wait(ctx)
			if err != nil || r.Usage.Known {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}
func TestComposedSteerDoesNotContinueAfterNaturalCompletion(t *testing.T) {
	s, w := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		finishClaude(s, false)
		return json.RawMessage(`{}`), nil
	}
	_, err = s.Steer(ctx, turn.ID(), Input{"redirect"}, SteerOptions{})
	if !errors.Is(err, ErrStaleTurn) {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.calls) != 2 {
		t.Fatal("sent followup despite natural completion")
	}
}

func TestCancelledControlDoesNotWaitForAnotherControl(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	if err := s.lockOp(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.unlockOp()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.StartTurn(ctx, Input{"must not send"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestInterruptedUsageRemainsUnknown(t *testing.T) {
	s, w := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
		notify(s, `{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input_tokens":0,"output_tokens":0}}`)
		return json.RawMessage(`{}`), nil
	}
	if err = s.Interrupt(ctx, turn.ID()); err != nil {
		t.Fatal(err)
	}
	r, err := turn.Wait(ctx)
	if err != nil || r.Usage.Known {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestDefinitiveRejectionPreservesSession(t *testing.T) {
	for _, operation := range []string{"start", "steer", "interrupt"} {
		t.Run(operation, func(t *testing.T) {
			s, w := fakeSession(t, harness.Codex)
			ctx := testContext(t)
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
			}
			var turn *Turn
			var err error
			if operation != "start" {
				turn, err = s.StartTurn(ctx, Input{"initial"})
				if err != nil {
					t.Fatal(err)
				}
			}
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				if operation == "interrupt" {
					notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
				}
				return nil, ErrRejected
			}
			switch operation {
			case "start":
				_, err = s.StartTurn(ctx, Input{"rejected"})
			case "steer":
				_, err = s.Steer(ctx, turn.ID(), Input{"rejected"}, SteerOptions{})
			case "interrupt":
				err = s.Interrupt(ctx, turn.ID())
			}
			if !errors.Is(err, ErrRejected) {
				t.Fatal(err)
			}
			if operation == "steer" {
				notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
			}
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				return json.RawMessage(`{"turn":{"id":"turn-2"}}`), nil
			}
			next, err := s.StartTurn(ctx, Input{"new task"})
			if err != nil || next.ID() != "turn-2" {
				t.Fatalf("session lost after rejection: %v", err)
			}
		})
	}
}
func TestCodexExactToolTypes(t *testing.T) {
	for _, typ := range []string{"collabAgentToolCall", "subAgentActivity", "sleep", "imageGeneration"} {
		t.Run(typ, func(t *testing.T) {
			s, w := fakeSession(t, harness.Codex)
			ctx := testContext(t)
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				return json.RawMessage(`{"turn":{"id":"turn-1"}}`), nil
			}
			turn, err := s.StartTurn(ctx, Input{"start"})
			if err != nil {
				t.Fatal(err)
			}
			event := map[string]any{"method": "item/started", "params": map[string]any{"threadId": "session-1", "turnId": "turn-1", "item": map[string]any{"id": "item-1", "type": typ}}}
			notify(s, string(mustMarshal(event)))
			select {
			case e := <-turn.Events():
				if e.Kind != "tool_started" || e.Tool != typ {
					t.Fatalf("%+v", e)
				}
			case <-ctx.Done():
				t.Fatal("tool event missing")
			}
		})
	}
}
func TestCorrelatedClaudeInterruptPreservesNativeFailure(t *testing.T) {
	for _, subtype := range []string{"error_during_execution", "error_max_turns"} {
		t.Run(subtype, func(t *testing.T) {
			s, w := fakeSession(t, harness.Claude)
			ctx := testContext(t)
			turn, err := s.StartTurn(ctx, Input{"start"})
			if err != nil {
				t.Fatal(err)
			}
			w.requestFn = func(string, map[string]any) (json.RawMessage, error) {
				notify(s, string(mustMarshal(map[string]any{"type": "result", "subtype": subtype, "is_error": true})))
				return json.RawMessage(`{}`), nil
			}
			interruptErr := s.Interrupt(ctx, turn.ID())
			r, err := turn.Wait(ctx)
			if !r.NativeError {
				t.Fatal("native failure lost")
			}
			if subtype == "error_during_execution" {
				if interruptErr != nil || err != nil || r.Status != "interrupted" {
					t.Fatalf("%+v %v %v", r, err, interruptErr)
				}
			} else if !errors.Is(err, ErrTurnFailed) || !errors.Is(interruptErr, ErrTurnFailed) {
				t.Fatalf("unrelated failure masked: %+v %v %v", r, err, interruptErr)
			}
		})
	}
}

// Policy defaults and the per-engine allowed values, judged without resolving
// any path or reading the environment.
func TestNormalizePolicyDefaultsAndRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		in   Options
		want Policy
		ok   bool
	}{
		"codex defaults":        {Options{Provider: harness.Provider{Engine: harness.Codex}}, Policy{CodexSandbox: "read-only", CodexApproval: "never", ClaudePermission: "dontAsk"}, true},
		"codex chosen":          {Options{Provider: harness.Provider{Engine: harness.Codex}, Policy: Policy{CodexSandbox: "workspace-write", CodexApproval: "untrusted"}}, Policy{CodexSandbox: "workspace-write", CodexApproval: "untrusted", ClaudePermission: "dontAsk"}, true},
		"codex bad sandbox":     {Options{Provider: harness.Provider{Engine: harness.Codex}, Policy: Policy{CodexSandbox: "everything"}}, Policy{}, false},
		"codex bad approval":    {Options{Provider: harness.Provider{Engine: harness.Codex}, Policy: Policy{CodexApproval: "always"}}, Policy{}, false},
		"claude defaults":       {Options{Provider: harness.Provider{Engine: harness.Claude}}, Policy{CodexSandbox: "read-only", CodexApproval: "never", ClaudePermission: "dontAsk"}, true},
		"claude chosen":         {Options{Provider: harness.Provider{Engine: harness.Claude}, Policy: Policy{ClaudePermission: "plan"}}, Policy{CodexSandbox: "read-only", CodexApproval: "never", ClaudePermission: "plan"}, true},
		"claude bad permission": {Options{Provider: harness.Provider{Engine: harness.Claude}, Policy: Policy{ClaudePermission: "bypassPermissions"}}, Policy{}, false},
		// A field only the other engine reads would be silently ignored, so it
		// is refused instead — even a value that engine would accept.
		"claude refuses codex sandbox":  {Options{Provider: harness.Provider{Engine: harness.Claude}, Policy: Policy{CodexSandbox: "read-only"}}, Policy{}, false},
		"claude refuses codex approval": {Options{Provider: harness.Provider{Engine: harness.Claude}, Policy: Policy{CodexApproval: "never"}}, Policy{}, false},
		"codex refuses claude mode":     {Options{Provider: harness.Provider{Engine: harness.Codex}, Policy: Policy{ClaudePermission: "dontAsk"}}, Policy{}, false},
		"codex refuses claude tools":    {Options{Provider: harness.Provider{Engine: harness.Codex}, Policy: Policy{ClaudeTools: []string{}}}, Policy{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			// The order normalize applies them in: the caller's own fields are
			// judged before any default is filled in.
			err := otherEnginePolicy(tc.in)
			got := tc.in
			if err == nil {
				got, err = normalizePolicy(tc.in)
			}
			if (err == nil) != tc.ok {
				t.Fatalf("want ok=%v, got %v", tc.ok, err)
			}
			if tc.ok && !reflect.DeepEqual(got.Policy, tc.want) {
				t.Fatalf("got %+v, want %+v", got.Policy, tc.want)
			}
		})
	}
	tools := []string{"Read"}
	got, err := normalizePolicy(Options{Provider: harness.Provider{Engine: harness.Claude}, Policy: Policy{ClaudeTools: tools}})
	if err != nil {
		t.Fatal(err)
	}
	tools[0] = "Bash"
	if got.Policy.ClaudeTools[0] != "Read" {
		t.Fatal("the caller's tool list was not frozen")
	}
}
