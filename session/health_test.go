package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// Health describes a process, never progress. A session that has said nothing
// recently is unknown, and the library must not turn that into a verdict.
func TestHealthDistinguishesQuietFromExited(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	s.options.QuietAfter = 50 * time.Millisecond
	if got := s.Health(); got.State != Idle {
		t.Fatalf("a session with no turn is %q", got.State)
	}
	turn, err := s.StartTurn(ctx, Input{"work"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"stream_event","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"thinking"}}}`)
	health := s.Health()
	if health.State != Active || health.TurnID != turn.ID() {
		t.Fatalf("a streaming turn is %q: %+v", health.State, health)
	}
	time.Sleep(80 * time.Millisecond)
	if got := s.Health(); got.State != Quiet {
		t.Fatalf("a silent live turn is %q, want quiet", got.State)
	}
	if got := s.Health(); got.Reason != "" {
		t.Errorf("quiet must carry no verdict, got reason %q", got.Reason)
	}
	code := 1
	s.fail(&ProcessError{Engine: "claude", Code: ProcessExited, ExitCode: &code})
	health = s.Health()
	if health.State != Exited || health.Reason != ProcessExited {
		t.Fatalf("a dead harness is %q/%q", health.State, health.Reason)
	}
}

func TestHealthReportsExplicitProviderFailureSeparately(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	s.fail(ErrProtocol)
	health := s.Health()
	if health.State != Failed || health.Reason != "protocol" {
		t.Fatalf("an explicit failure is %q/%q", health.State, health.Reason)
	}
}

func TestProcessErrorCarriesExitStatusAndStaysATransportFailure(t *testing.T) {
	code := 127
	err := &ProcessError{Engine: "codex", Code: ProcessExited, ExitCode: &code}
	if !strings.Contains(err.Error(), "127") || !strings.Contains(err.Error(), "codex") {
		t.Errorf("exit status is not reported: %s", err)
	}
	if got := err.Unwrap(); got != ErrTransport {
		t.Errorf("a dead harness must still classify as a transport failure, got %v", got)
	}
	signalled := &ProcessError{Engine: "claude", Code: ProcessSignalled}
	if !strings.Contains(signalled.Error(), "signal") {
		t.Errorf("a signalled harness is not described: %s", signalled)
	}
}

// Captured harness output is evidence for an operator's private record. It must
// not carry control sequences, must be bounded, and must not carry a credential.
func TestSanitizeBoundsAndRedactsCapturedOutput(t *testing.T) {
	raw := []byte("\x1b[31merror\x1b[0m: request failed\nAuthorization: Bearer sk-abc123DEFghi456JKLmno789\n\x00")
	got := sanitize(raw, 2048)
	for _, forbidden := range []string{"\x1b", "\x00", "\n", "sk-abc123DEFghi456JKLmno789"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitized output still contains %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "request failed") {
		t.Errorf("sanitizing removed the diagnostic itself: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("a credential-shaped run was not redacted: %q", got)
	}
	long := sanitize([]byte(strings.Repeat("detail ", 2000)), 64)
	if len(long) > 160 || !strings.Contains(long, "truncated") {
		t.Errorf("captured output was not bounded with a marker: %d bytes", len(long))
	}
	if got = sanitize([]byte("model provider returned 503 after 2 attempts"), 2048); !strings.Contains(got, "503") {
		t.Errorf("ordinary prose was over-redacted: %q", got)
	}
}

// A turn makes many requests. A caller holding a budget has to see them as they
// happen; waiting for the terminal figure is waiting until it is too late.
func TestUsageIsPublishedPerModelResponse(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"work"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"m1","model":"claude","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}}`)
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"m2","model":"claude","content":[{"type":"text","text":"two"}],"usage":{"input_tokens":20,"output_tokens":6,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
	interim := []Usage{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range turn.Events() {
			if event.Kind == "usage" {
				interim = append(interim, *event.Usage)
			}
		}
	}()
	finishClaude(s, false)
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-drained
	if len(interim) < 3 {
		t.Fatalf("usage was not published during the turn: %+v", interim)
	}
	if interim[0].Final || interim[1].Final {
		t.Errorf("a per-response observation was published as the turn's accounting: %+v", interim)
	}
	// Claude's input_tokens leaves out cache reads; the shared Input counts them.
	if interim[1].Input != 32 || interim[1].CacheRead != 2 || !interim[1].CacheKnown || interim[1].Output != 10 {
		t.Errorf("per-response observations were not accumulated: %+v", interim[1])
	}
	last := interim[len(interim)-1]
	// The terminal report omitted cache writes, so its split is not known.
	if !last.Final || last.Input != 52 || last.CacheRead != 40 || last.CacheKnown {
		t.Errorf("the turn's own accounting was not published as final: %+v", last)
	}
	if result.Observed.Input != 32 || result.Observed.Output != 10 || !result.Observed.Known {
		t.Errorf("observations were not retained on the result: %+v", result.Observed)
	}
	if result.Usage.Input != 52 || !result.Usage.Known {
		t.Errorf("terminal accounting was lost: %+v", result.Usage)
	}
}

// A failed turn consumed something. Keeping the observations as evidence is not
// the same as claiming they measure the turn, and the distinction is the point.
func TestFailedTurnKeepsObservedUsageWithoutClaimingItMeasuresTheTurn(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	ctx := testContext(t)
	turn, err := s.StartTurn(ctx, Input{"work"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range turn.Events() {
		}
	}()
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"m1","model":"claude","content":[{"type":"text","text":"partial"}],"usage":{"input_tokens":55,"output_tokens":9,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
	finishClaude(s, true)
	result, waitErr := turn.Wait(ctx)
	if waitErr == nil {
		t.Fatal("a failed turn reported success")
	}
	if result.Usage.Known {
		t.Errorf("a failed turn's counters were promoted to a measurement: %+v", result.Usage)
	}
	if !result.Observed.Known || result.Observed.Input != 55 || result.Observed.Output != 9 {
		t.Errorf("what the failed turn was seen to consume was discarded: %+v", result.Observed)
	}
}

// Every engine reports in the shared shape: Input is the whole prompt, and the
// cache figures are parts of it that are known only when the provider split them.
func TestUsageIsReportedInTheSharedShape(t *testing.T) {
	claude := parseClaudeUsage(json.RawMessage(`{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}`))
	if !claude.Known || claude.Input != 130 || claude.CacheRead != 100 || claude.CacheWrite != 20 || !claude.CacheKnown {
		t.Errorf("Claude's cache figures were not folded into Input: %+v", claude)
	}
	if fresh, ok := claude.Fresh(); !ok || fresh != 10 {
		t.Errorf("fresh input was not recoverable: %d %v", fresh, ok)
	}
	partial := parseClaudeUsage(json.RawMessage(`{"input_tokens":10,"output_tokens":5}`))
	if !partial.Known || partial.Input != 10 || partial.CacheKnown {
		t.Errorf("an unreported split was presented as uncached: %+v", partial)
	}
	if huge := parseClaudeUsage(json.RawMessage(`{"input_tokens":9223372036854775000,"output_tokens":1,"cache_read_input_tokens":9223372036854775000,"cache_creation_input_tokens":0}`)); huge.Known {
		t.Errorf("an unrepresentable prompt total was reported as known: %+v", huge)
	}
	codex, ok := parseCodexUsage(json.RawMessage(`{"inputTokens":100,"cachedInputTokens":80,"outputTokens":10,"reasoningOutputTokens":4}`))
	if got := codex.normalized(); !ok || got.Input != 100 || got.CacheRead != 80 || !got.CacheKnown || got.Reasoning != 4 {
		t.Errorf("Codex usage was not taken as the whole prompt: %+v", got)
	}
	if _, ok := parseCodexUsage(json.RawMessage(`{"inputTokens":100,"cachedInputTokens":80,"cacheWriteInputTokens":30,"outputTokens":10,"reasoningOutputTokens":0}`)); ok {
		t.Error("cache figures larger than the prompt were accepted")
	}
	split := Usage{Usage: harness.Usage{Known: true, Input: 5, CacheKnown: true}}
	unsplit := Usage{Usage: harness.Usage{Known: true, Input: 5}}
	if got := (Usage{}).add(split); !got.CacheKnown {
		t.Errorf("a first split observation lost its split: %+v", got)
	}
	if got := (Usage{}).add(split).add(unsplit); got.CacheKnown || got.Input != 10 {
		t.Errorf("an accumulation with an unsplit response kept a known split: %+v", got)
	}
	thought := Usage{Usage: harness.Usage{Known: true, Output: 5, Reasoning: 2, ReasoningKnown: true}}
	silent := Usage{Usage: harness.Usage{Known: true, Output: 5}}
	if got := (Usage{}).add(thought).add(thought); !got.ReasoningKnown || got.Reasoning != 4 {
		t.Errorf("reasoning reported by every response was lost: %+v", got)
	}
	if got := (Usage{}).add(thought).add(silent); got.ReasoningKnown {
		t.Errorf("a response that did not report reasoning left it known: %+v", got)
	}
	thinking := parseClaudeUsage(json.RawMessage(`{"input_tokens":3,"output_tokens":9,"output_tokens_details":{"thinking_tokens":4}}`))
	if !thinking.ReasoningKnown || thinking.Reasoning != 4 {
		t.Errorf("Claude's thinking was not read as reasoning: %+v", thinking)
	}
}

func TestUsageAccumulationRefusesToWrap(t *testing.T) {
	near := Usage{Usage: harness.Usage{Known: true, Input: maxInt64 - 1}}
	if got := near.add(Usage{Usage: harness.Usage{Known: true, Input: 5}}); got.Known {
		t.Errorf("an unrepresentable total was reported as known: %+v", got)
	}
	if got := near.add(Usage{}); got != near {
		t.Errorf("an unknown observation changed the accumulation: %+v", got)
	}
	if got := (Usage{}).add(Usage{Usage: harness.Usage{Known: true, Output: 7}}); !got.Known || got.Output != 7 {
		t.Errorf("a first observation was lost: %+v", got)
	}
}

// The startup cross-check runs on the frame the harness emits before any
// prompt, so a mismatch still precedes inference — and it ends the session.
func TestAdvertisedToolMismatchClosesTheSession(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	s.options.Restriction = &Restriction{Tools: ToolHost{Server: "agent_workspace", Tools: []ToolDefinition{{Name: "read_file"}}}}
	notify(s, `{"type":"system","subtype":"init","session_id":"session-1","mcp_servers":[{"name":"agent_workspace","status":"connected"}],"tools":["mcp__agent_workspace__read_file","Bash"]}`)
	if s.Health().State != Exited && s.Health().State != Failed {
		t.Fatalf("session survived an unauthorized tool surface: %+v", s.Health())
	}
	if s.Capabilities().RestrictTools.Availability != harness.Unsupported {
		t.Errorf("capability was not recorded as unsupported: %+v", s.Capabilities().RestrictTools)
	}
}

func TestAdvertisedToolMatchRecordsTheCapability(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	s.options.Restriction = &Restriction{Tools: ToolHost{Server: "agent_workspace", Tools: []ToolDefinition{{Name: "read_file"}, {Name: "finish"}}}}
	notify(s, `{"type":"system","subtype":"init","session_id":"session-1","mcp_servers":[{"name":"agent_workspace","source":"dynamic","status":"connected"}],"tools":["mcp__agent_workspace__finish","mcp__agent_workspace__read_file"]}`)
	if s.Capabilities().RestrictTools.Availability != harness.Native {
		t.Fatalf("a matching surface was not recorded: %+v", s.Capabilities().RestrictTools)
	}
	if s.Health().State == Exited || s.Health().State == Failed {
		t.Fatal("a matching surface closed the session")
	}
}

// Observed for real against the installed CLI with a reserved server name: the
// server is accepted, silently not loaded, and the session runs with no tools
// while reporting success. A session with nothing to work with is not a working
// session, so this ends it too.
func TestSilentlyDroppedToolServerClosesTheSession(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	s.options.Restriction = &Restriction{Tools: ToolHost{Server: "agent_workspace", Tools: []ToolDefinition{{Name: "read_file"}}}}
	notify(s, `{"type":"system","subtype":"init","session_id":"session-1","mcp_servers":[],"tools":[]}`)
	if s.Health().State != Exited && s.Health().State != Failed {
		t.Fatalf("session survived without its tool server: %+v", s.Health())
	}
	if s.Capabilities().RestrictTools.Availability != harness.Unsupported {
		t.Fatalf("a dropped server was not recorded as unsupported: %+v", s.Capabilities().RestrictTools)
	}
}

// A reserved name is refused before anything is launched, so an operator is
// told what to change rather than seeing an empty session.
func TestReservedToolServerNameIsRefused(t *testing.T) {
	o := restrictedOptions(t, harness.Claude)
	o.Restriction = &Restriction{Tools: o.Restriction.Tools}
	o.Restriction.Tools.Server = "workspace"
	_, err := normalize(o)
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityServerNameReserved {
		t.Fatalf("a reserved tool server name was accepted: %v", err)
	}
	if len(failure.Tools) != 1 || failure.Tools[0] != "workspace" {
		t.Errorf("refusal did not name the server: %v", failure.Tools)
	}
	// Codex has no such reservation, so the same name is fine there.
	codex := restrictedOptions(t, harness.Codex)
	codex.Restriction = &Restriction{Tools: codex.Restriction.Tools}
	codex.Restriction.Tools.Server = "workspace"
	if _, err = normalize(codex); err != nil {
		t.Fatalf("a Claude-only reservation was applied to Codex: %v", err)
	}
}

// An unrestricted session is not subject to any of this.
func TestUnrestrictedSessionIgnoresTheToolCrossCheck(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"type":"system","subtype":"init","tools":["Bash","Read"]}`), &frame); err != nil {
		t.Fatal(err)
	}
	if s.observeClaudeInit(frame) {
		t.Fatal("an unrestricted session consumed the init frame as a capability check")
	}
	if s.Health().State == Failed {
		t.Fatal("an unrestricted session was closed by a tool advertisement")
	}
}
