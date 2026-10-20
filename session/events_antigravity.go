package session

import (
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
	"github.com/shhac/lib-agent-harness/internal/tokens"
)

// Antigravity's stream-json turn events (`step_update`, `error`, `result`).

const (
	antigravityTurnFailed         = "turn_failed"
	antigravityCanceled           = "canceled"
	antigravityInterrupted        = "interrupted"
	antigravityInvalid            = "invalid"
	antigravityStatusUnrecognized = "status_unrecognized"
)

var antigravityToolIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

func (s *Session) antigravityEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if id := antigravityConversation(m); id != "" && ref.ID != "" && id != ref.ID {
		s.fail(ErrProtocol)
		return
	}
	switch str(m, "event") {
	case "step_update":
		s.antigravityStepUpdate(t, m["step_update"])
	case "error":
		if report := s.options.OnDiagnostic; report != nil {
			report(Diagnostic{Engine: harness.Antigravity, Stage: "turn_error", Code: "error_event", Detail: sanitize(m["error"], 1024), At: time.Now().UTC()})
		}
	case "result":
		s.antigravityResult(t, m["result"])
	}
}

// antigravityConversation is the frame's conversation: agy 1.4.2 states it
// inside step_update and result payloads, and only init carries it at the top.
func antigravityConversation(m map[string]json.RawMessage) string {
	if id := str(m, "conversation_id"); id != "" {
		return id
	}
	for _, key := range []string{"step_update", "result"} {
		var payload struct {
			ConversationID string `json:"conversation_id"`
		}
		if json.Unmarshal(m[key], &payload) == nil && payload.ConversationID != "" {
			return payload.ConversationID
		}
	}
	return ""
}

func (s *Session) antigravityStepUpdate(t *Turn, raw json.RawMessage) {
	var u struct {
		StepIndex *int            `json:"step_index"`
		StepType  string          `json:"step_type"`
		State     string          `json:"state"`
		TextDelta string          `json:"text_delta"`
		ToolName  string          `json:"tool_name"`
		Usage     json.RawMessage `json:"usage"`
		ToolInfo  *struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
			Output     string          `json:"output"`
			Error      *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"tool_info"`
	}
	if json.Unmarshal(raw, &u) != nil || u.StepIndex == nil || *u.StepIndex < 0 {
		return
	}
	itemID := "step-" + strconv.Itoa(*u.StepIndex)
	switch u.StepType {
	case "agent_response":
		if u.TextDelta != "" {
			s.text(t, itemID, u.TextDelta, false)
		}
		if u.State == "DONE" {
			s.antigravityStepUsage(t, itemID, u.Usage)
		}
	case "tool":
		name := u.ToolName
		var input json.RawMessage
		var output string
		if u.ToolInfo != nil {
			if name == "" {
				name = u.ToolInfo.Name
			}
			if !rawjson.Absent(u.ToolInfo.Parameters) {
				input = u.ToolInfo.Parameters
			}
			output = u.ToolInfo.Output
			if output == "" && u.ToolInfo.Error != nil {
				output = u.ToolInfo.Error.Message
			}
		}
		if !antigravityToolIdentifier.MatchString(name) {
			name = "tool"
		}
		t.mu.Lock()
		if t.antigravityTools == nil {
			t.antigravityTools = map[string]bool{}
		}
		started := t.antigravityTools[itemID]
		if !started {
			t.antigravityTools[itemID] = true
		}
		t.mu.Unlock()
		if !started {
			s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: itemID, Tool: name, Status: "in_progress"}, input, ""))
		}
		switch u.State {
		case "DONE", "ERROR", "CANCELED", "INTERRUPTED":
			doneKey := itemID + ":done"
			t.mu.Lock()
			alreadyDone := t.antigravityTools[doneKey]
			if !alreadyDone {
				t.antigravityTools[doneKey] = true
			}
			t.mu.Unlock()
			if !alreadyDone {
				status := "completed"
				if u.State != "DONE" {
					status = "failed"
				}
				s.emit(t, s.withToolPayload(Event{Kind: "tool_completed", ItemID: itemID, Tool: name, Status: status}, input, output))
			}
		}
	}
}

// antigravityStepUsage observes each response once. A response without both
// counts leaves the turn's settled usage unknown rather than undercounted.
func (s *Session) antigravityStepUsage(t *Turn, itemID string, raw json.RawMessage) {
	usage := parseAntigravityUsage(raw)
	t.mu.Lock()
	if t.antigravityTools == nil {
		t.antigravityTools = map[string]bool{}
	}
	counted := t.antigravityTools[itemID+":usage"]
	t.antigravityTools[itemID+":usage"] = true
	if !counted && !usage.Known {
		t.antigravityUsageGap = true
	}
	t.mu.Unlock()
	if !counted {
		s.observeRequestUsage(t, usage)
	}
}

func (s *Session) antigravityResult(t *Turn, raw json.RawMessage) {
	var r struct {
		Status   string          `json:"status"`
		Response string          `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Status == "" {
		s.fail(ErrProtocol)
		return
	}
	status, code := antigravityOutcome(r.Status, !rawjson.Absent(r.Error))
	if status == "completed" && r.Response != "" {
		s.text(t, "result", r.Response, true)
	}
	var err error
	t.mu.Lock()
	var usage Usage
	if status == "completed" && !t.antigravityUsageGap {
		usage = t.result.Observed
	}
	if usage.Known {
		usage.Final = true
		t.result.Usage = usage
	}
	if code != "" {
		t.result.NativeError = true
		err = &TurnError{Engine: harness.Antigravity, Code: code}
	}
	t.mu.Unlock()
	if code != "" && !rawjson.Absent(r.Error) {
		if report := s.options.OnDiagnostic; report != nil {
			report(Diagnostic{Engine: harness.Antigravity, Stage: "turn_result", Code: code, Detail: sanitize(r.Error, 1024), At: time.Now().UTC()})
		}
	}
	if usage.Known {
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
}

func antigravityOutcome(status string, hasError bool) (string, string) {
	switch status {
	case "SUCCESS":
		if hasError {
			return "failed", antigravityTurnFailed
		}
		return "completed", ""
	case "CANCELED":
		return "failed", antigravityCanceled
	case "INTERRUPTED":
		return "failed", antigravityInterrupted
	case "ERROR":
		return "failed", antigravityTurnFailed
	case "INVALID":
		return "failed", antigravityInvalid
	}
	return "failed", antigravityStatusUnrecognized
}

func parseAntigravityUsage(raw json.RawMessage) Usage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return Usage{}
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if rawjson.Absent(fields[key]) {
			return Usage{}
		}
	}
	var c struct {
		Input     *int64 `json:"input_tokens"`
		Output    *int64 `json:"output_tokens"`
		Reasoning *int64 `json:"thinking_tokens"`
		Read      *int64 `json:"cache_read_tokens"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	output := c.Output
	if c.Reasoning != nil {
		output = tokens.Sum(c.Output, c.Reasoning)
	}
	return Usage{Usage: tokens.CacheExclusive(tokens.Report{Input: c.Input, Output: output, CacheRead: c.Read, CacheWrite: tokens.NoCacheWrite(c.Read), Reasoning: c.Reasoning})}
}
