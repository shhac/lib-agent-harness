package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Health describes a process, never progress. A session that has said nothing
// recently is unknown, and the library must not turn that into a verdict.
func TestHealthDistinguishesQuietFromExited(t *testing.T) {
	s, _ := fakeSession(t, Claude)
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
	s, _ := fakeSession(t, Claude)
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
	s, _ := fakeSession(t, Claude)
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
	if interim[1].Input != 30 || interim[1].Output != 10 {
		t.Errorf("per-response observations were not accumulated: %+v", interim[1])
	}
	last := interim[len(interim)-1]
	if !last.Final || last.Input != 12 {
		t.Errorf("the turn's own accounting was not published as final: %+v", last)
	}
	if result.Observed.Input != 30 || result.Observed.Output != 10 || !result.Observed.Known {
		t.Errorf("observations were not retained on the result: %+v", result.Observed)
	}
	if result.Usage.Input != 12 || !result.Usage.Known {
		t.Errorf("terminal accounting was lost: %+v", result.Usage)
	}
}

// A failed turn consumed something. Keeping the observations as evidence is not
// the same as claiming they measure the turn, and the distinction is the point.
func TestFailedTurnKeepsObservedUsageWithoutClaimingItMeasuresTheTurn(t *testing.T) {
	s, _ := fakeSession(t, Claude)
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

func TestUsageAccumulationRefusesToWrap(t *testing.T) {
	near := Usage{Known: true, Input: maxInt64 - 1}
	if got := near.add(Usage{Known: true, Input: 5}); got.Known {
		t.Errorf("an unrepresentable total was reported as known: %+v", got)
	}
	if got := near.add(Usage{}); got != near {
		t.Errorf("an unknown observation changed the accumulation: %+v", got)
	}
	if got := (Usage{}).add(Usage{Known: true, Output: 7}); !got.Known || got.Output != 7 {
		t.Errorf("a first observation was lost: %+v", got)
	}
}

// The startup cross-check runs on the frame the harness emits before any
// prompt, so a mismatch still precedes inference — and it ends the session.
func TestAdvertisedToolMismatchClosesTheSession(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	s.options.Restriction = &Restriction{Tools: ToolHost{Server: "workspace", Tools: []ToolDefinition{{Name: "read_file"}}}}
	notify(s, `{"type":"system","subtype":"init","session_id":"session-1","tools":["mcp__workspace__read_file","Bash"]}`)
	if s.Health().State != Exited && s.Health().State != Failed {
		t.Fatalf("session survived an unauthorized tool surface: %+v", s.Health())
	}
	if s.Capabilities().RestrictTools.Availability != Unsupported {
		t.Errorf("capability was not recorded as unsupported: %+v", s.Capabilities().RestrictTools)
	}
}

func TestAdvertisedToolMatchRecordsTheCapability(t *testing.T) {
	s, _ := fakeSession(t, Claude)
	s.options.Restriction = &Restriction{Tools: ToolHost{Server: "workspace", Tools: []ToolDefinition{{Name: "read_file"}, {Name: "finish"}}}}
	notify(s, `{"type":"system","subtype":"init","session_id":"session-1","tools":["mcp__workspace__finish","mcp__workspace__read_file"]}`)
	if s.Capabilities().RestrictTools.Availability != Native {
		t.Fatalf("a matching surface was not recorded: %+v", s.Capabilities().RestrictTools)
	}
	if s.Health().State == Exited || s.Health().State == Failed {
		t.Fatal("a matching surface closed the session")
	}
}

// An unrestricted session is not subject to any of this.
func TestUnrestrictedSessionIgnoresTheToolCrossCheck(t *testing.T) {
	s, _ := fakeSession(t, Claude)
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
