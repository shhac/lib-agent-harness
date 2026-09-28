package session

import (
	"encoding/json"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
)

// Codex's app-server dialect. Kept apart from Claude's because the two
// protocols evolve independently; in particular the usage rule here (sum each
// model response within this turn) is the opposite of Claude's.

func (s *Session) codexEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	var p map[string]json.RawMessage
	if json.Unmarshal(m["params"], &p) != nil {
		return
	}
	if thread := str(p, "threadId"); thread != "" && ref.ID != "" && thread != ref.ID {
		return
	}
	method := str(m, "method")
	if method == "turn/started" {
		s.compactTurnStarted(t, p)
		return
	}
	t.mu.Lock()
	waitingID := t.awaitingCompactID
	t.mu.Unlock()
	if waitingID {
		return
	}
	if id := str(p, "turnId"); id != "" && id != t.ID() {
		return
	}
	switch method {
	case "item/agentMessage/delta":
		s.text(t, str(p, "itemId"), str(p, "delta"), false)
	case "item/started", "item/completed":
		s.codexItem(t, method == "item/completed", p)
	case "thread/tokenUsage/updated":
		s.codexTokenUsage(t, p)
	case "thread/compacted":
		// Deprecated in favour of the contextCompaction item, and still sent.
		s.invalidateContext(t, "context compacted; awaiting a fresh observation")
		s.markContext(ContextCompacted)
	case "turn/completed":
		s.codexTurnCompleted(t, p)
	}
}

func (s *Session) codexItem(t *Turn, completed bool, p map[string]json.RawMessage) {
	var item map[string]json.RawMessage
	if json.Unmarshal(p["item"], &item) != nil {
		return
	}
	typ := str(item, "type")
	if typ == "contextCompaction" {
		s.invalidateContext(t, "context compacting; awaiting a fresh observation")
		kind := "compaction_started"
		if completed {
			kind = "compaction_completed"
			s.markContext(ContextCompacted)
		}
		s.emit(t, Event{Kind: kind, ItemID: str(item, "id")})
		return
	}
	id := str(item, "id")
	if typ == "agentMessage" {
		if completed {
			s.text(t, id, str(item, "text"), true)
		}
		return
	}
	switch typ {
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "subAgentActivity", "sleep", "imageGeneration", "webSearch", "imageView":
		tool := typ
		if name := str(item, "tool"); name != "" {
			tool = name
		}
		input, output, exit := codexToolActivity(typ, item)
		event := Event{Kind: "tool_started", ItemID: id, Tool: tool, Status: str(item, "status")}
		if completed {
			event.Kind, event.ExitCode = "tool_completed", exit
		} else {
			output = ""
		}
		s.emit(t, s.withToolPayload(event, input, output))
	}
}

// codexToolActivity reads what an item was asked to do and what it produced,
// from the ThreadItem shapes codex-cli 0.156.1's app-server protocol declares
// (codex app-server generate-ts). A completed item restates the call, so its
// input is reported again on completion.
func codexToolActivity(typ string, item map[string]json.RawMessage) (json.RawMessage, string, *int) {
	switch typ {
	case "commandExecution":
		var exit *int
		_ = json.Unmarshal(item["exitCode"], &exit)
		return pickFields(item, "command", "cwd"), str(item, "aggregatedOutput"), exit
	case "fileChange":
		return pickFields(item, "changes"), "", nil
	case "mcpToolCall":
		var failure struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(item["error"], &failure) == nil && failure.Message != "" {
			return item["arguments"], failure.Message, nil
		}
		var result struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(item["result"], &result)
		return item["arguments"], toolResultText(result.Content), nil
	case "dynamicToolCall":
		var items []struct{ Type, Text string }
		_ = json.Unmarshal(item["contentItems"], &items)
		var parts []string
		for _, content := range items {
			if content.Type == "inputText" {
				parts = append(parts, content.Text)
			}
		}
		return item["arguments"], strings.Join(parts, "\n"), nil
	case "webSearch":
		output := ""
		if !jsonAbsent(item["results"]) {
			output = string(item["results"])
		}
		return pickFields(item, "query", "action"), output, nil
	case "imageView":
		return pickFields(item, "path"), "", nil
	case "collabAgentToolCall":
		return pickFields(item, "prompt", "model", "receiverThreadIds"), "", nil
	}
	return nil, "", nil
}

func (s *Session) codexTokenUsage(t *Turn, p map[string]json.RawMessage) {
	if str(p, "turnId") == "" {
		return
	}
	if c, err := parseCodexContext(p["tokenUsage"]); err == nil {
		s.observeContext(c, t)
	}
	var envelope struct {
		Last  json.RawMessage `json:"last"`
		Total json.RawMessage `json:"total"`
	}
	if json.Unmarshal(p["tokenUsage"], &envelope) != nil {
		return
	}
	last, lastOK := parseCodexUsage(envelope.Last)
	total, totalOK := parseCodexUsage(envelope.Total)
	if !lastOK || !totalOK {
		return
	}
	// last is one model response; total identifies repeated notifications. Sum
	// response usage within this turn, never the resumed session's old total.
	t.mu.Lock()
	fresh := t.lastCodexTotal != total || !t.result.Usage.Known
	if fresh {
		t.result.Usage = t.result.Usage.add(last.normalized())
		t.lastCodexTotal = total
	}
	t.mu.Unlock()
	if fresh {
		s.observeRequestUsage(t, last.normalized())
	}
}

func (s *Session) codexTurnCompleted(t *Turn, p map[string]json.RawMessage) {
	var turn struct{ ID, Status string }
	if json.Unmarshal(p["turn"], &turn) != nil {
		return
	}
	if turn.ID != t.ID() {
		return
	}
	var err error
	if turn.Status == "failed" {
		t.mu.Lock()
		t.result.NativeError = true
		t.mu.Unlock()
		err = ErrTurnFailed
	}
	switch turn.Status {
	case "completed", "interrupted", "failed":
	default:
		s.fail(ErrProtocol)
		return
	}
	// The turn's own accounting is published separately from the per-response
	// observations, so a caller never has to guess which one it is holding.
	t.mu.Lock()
	accounting := t.result.Usage
	t.mu.Unlock()
	if accounting.Known {
		accounting.Final = true
		s.emit(t, Event{Kind: "usage", Usage: &accounting})
	}
	s.emit(t, Event{Kind: "status", Status: turn.Status})
	t.finish(turn.Status, err)
}

type codexUsage struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cachedInputTokens"`
	CacheWrite int64 `json:"cacheWriteInputTokens"`
	Reasoning  int64 `json:"reasoningOutputTokens"`
}

func parseCodexUsage(raw json.RawMessage) (codexUsage, bool) {
	var fields map[string]json.RawMessage
	var u codexUsage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &u) != nil {
		return u, false
	}
	for _, key := range []string{"inputTokens", "outputTokens", "cachedInputTokens", "reasoningOutputTokens"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return u, false
		}
	}
	return u, u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheRead <= u.Input && u.CacheWrite >= 0 && u.CacheWrite <= u.Input-u.CacheRead && u.Reasoning >= 0 && u.Reasoning <= u.Output
}

// normalized is already the shared shape: Codex's inputTokens counts every
// prompt token, cached ones included, and its cached figure is always reported.
func (u codexUsage) normalized() Usage {
	return Usage{Usage: harness.Usage{Known: true, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning, CacheKnown: true}}
}
