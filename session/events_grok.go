package session

import (
	"encoding/json"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/tokens"
)

// Grok's session/update dialect, and the prompt response that ends a turn.
// Kept apart from the other engines because its usage arrives in two scopes
// that must not be confused: each model response's figures stream as a
// response_completed notification, and the turn's own accounting is in the
// prompt's response. Every shape here was observed from grok 1.0.41.

// grokPermissionRejected is the turn failure code for a cancellation Grok says
// was a refused permission, beside the shared ACP codes in acp_turn.go and
// the refusal codes in grok.go.
const grokPermissionRejected = "permission_rejected"

// grokEvent routes one notification for the active turn. Streamed content is
// ACP's session/update; per-response usage is Grok's own extension envelope.
func (s *Session) grokEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	switch str(m, "method") {
	case "session/update":
		s.acpUpdate(t, ref, m["params"], grokUpdates)
	case "_x.ai/session_notification":
		if kind, update, ok := acpSessionUpdate(m["params"], ref); ok && kind == "response_completed" {
			s.grokResponseCompleted(t, update)
		}
	}
}

// grokUpdates names a turn's text by the model response it belongs to, which
// grokResponseCompleted advances.
var grokUpdates = acpUpdates{messageItem: acpCountedItem("response-"), toolName: grokToolName}

// grokResponseCompleted publishes one model response's usage while the turn
// runs, estimates the context from it, and closes that response's text.
func (s *Session) grokResponseCompleted(t *Turn, update map[string]json.RawMessage) {
	usage := parseGrokResponseUsage(update["usage"])
	s.observeRequestUsage(t, usage)
	s.grokContext(t, usage)
	t.mu.Lock()
	t.acpMessage++
	t.mu.Unlock()
}

// grokContext estimates occupancy from the latest response's whole input, and
// states capacity from the window Grok's model state gives for the session's
// model. Pending output and tool results are not counted, hence Estimated.
func (s *Session) grokContext(t *Turn, u Usage) {
	if !u.Known {
		return
	}
	s.mu.Lock()
	model, capacity := s.acpModel, s.grokCapacity
	s.mu.Unlock()
	used := u.Input
	c := ContextSnapshot{Model: model, UsedTokens: &used}
	c.Observation = observation("response_completed.usage", harness.Estimated)
	c.Reason = "latest model input; pending output and tool results are not counted"
	if capacity > 0 {
		c.CapacityTokens = &capacity
		c.ModelCapacityTokens = cloneValue(&capacity)
	}
	setContextPercent(&c)
	s.observeContext(c, t)
}

// grokToolName is the tool's own identifier, never its title, which carries
// paths and commands.
func grokToolName(update map[string]json.RawMessage) string {
	var meta struct {
		Tool struct {
			Name string `json:"name"`
		} `json:"x.ai/tool"`
	}
	_ = json.Unmarshal(update["_meta"], &meta)
	for _, name := range []string{meta.Tool.Name, str(update, "title")} {
		if acpToolIdentifier.MatchString(name) {
			return name
		}
	}
	return "tool"
}

// readGrokPromptEnd reads Grok's prompt response, which states the turn's
// usage and why a cancellation happened in its _meta.
func readGrokPromptEnd(body json.RawMessage, interruptRequested bool) (acpTurnEnd, bool) {
	var r struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Usage    json.RawMessage `json:"usage"`
			Category string          `json:"cancellationCategory"`
		} `json:"_meta"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		return acpTurnEnd{}, false
	}
	status, code := grokOutcome(r.StopReason, r.Meta.Category, interruptRequested)
	return acpTurnEnd{status: status, code: code, usage: parseGrokTurnUsage(r.Meta.Usage)}, true
}

// grokOutcome is the shared outcome, except that a cancellation nobody asked
// for which Grok says was a refused permission is reported as one.
func grokOutcome(stop, category string, interruptRequested bool) (string, string) {
	if stop == "cancelled" && !interruptRequested && category == "PermissionRejected" {
		return "failed", grokPermissionRejected
	}
	return acpOutcome(stop, interruptRequested)
}

// parseGrokTurnUsage reads the turn's accounting from a prompt response. Its
// inputTokens already counts cached input: across every observed turn it
// equals the sum of that turn's responses' uncached input and cache reads.
func parseGrokTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Read      *int64 `json:"cachedReadTokens"`
		Write     *int64 `json:"cacheCreationTokens"`
		Reasoning *int64 `json:"reasoningTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return Usage{}
	}
	return Usage{Usage: tokens.CacheInclusive(tokens.Report{Input: c.Input, Output: c.Output, CacheRead: c.Read, CacheWrite: c.Write, Reasoning: c.Reasoning})}
}

// parseGrokResponseUsage reads one response's figures, whose input_tokens
// excludes both cache figures, into the shared shape, whose Input includes
// them.
func parseGrokResponseUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"input_tokens"`
		Output    *int64 `json:"output_tokens"`
		Read      *int64 `json:"cache_read_input_tokens"`
		Write     *int64 `json:"cache_creation_input_tokens"`
		Reasoning *int64 `json:"reasoning_tokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return Usage{}
	}
	return Usage{Usage: tokens.CacheExclusive(tokens.Report{Input: c.Input, Output: c.Output, CacheRead: c.Read, CacheWrite: c.Write, Reasoning: c.Reasoning})}
}
