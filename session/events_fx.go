package session

// fx's session/update dialect, and the prompt response that ends a turn.
//
// Kept apart from the other engines because fx reports the context window
// and the occupancy in one notification, and because its tool calls name the
// tool in the update itself, without a separate meta block. Every shape here
// was observed from fx 0.0.14.

import (
	"encoding/json"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/tokens"
)

// readFxPromptEnd reads fx's prompt response, which states the turn's own
// usage beside its stop reason.
func readFxPromptEnd(body json.RawMessage, interruptRequested bool) (acpTurnEnd, bool) {
	var r struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		return acpTurnEnd{}, false
	}
	status, code := acpOutcome(r.StopReason, interruptRequested)
	return acpTurnEnd{status: status, code: code, usage: parseFxTurnUsage(r.Usage)}, true
}

// fxEvent routes one notification for the active turn.
func (s *Session) fxEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if str(m, "method") == "session/update" {
		s.acpUpdate(t, ref, m["params"], fxUpdates)
	}
}

// fxUpdates names each message by the stable id fx gives it, and otherwise
// starts a new one after each tool call. fx states the context window and
// the occupancy in one update, so the occupancy is measured rather than
// estimated.
var fxUpdates = acpUpdates{
	messageItem: func(t *Turn, update map[string]json.RawMessage) string {
		if id := str(update, "messageId"); id != "" {
			return id
		}
		return acpCountedItem("response-")(t, update)
	},
	newMessageAfterTool: true, toolName: fxToolName,
	context: func(s *Session, t *Turn, update map[string]json.RawMessage) {
		s.acpContext(t, update, harness.Measured, "fx's own count of the conversation's context")
	},
}

// fxToolName is the tool's own identifier, never its title, which carries
// paths and commands.
func fxToolName(update map[string]json.RawMessage) string {
	if name := str(update, "name"); acpToolIdentifier.MatchString(name) {
		return name
	}
	return ""
}

// parseFxTurnUsage reads the turn's accounting from a prompt response. Its
// inputTokens already counts cached input: on fx 0.0.14 a turn's input and
// output match the context its usage_update then states, with the cache reads
// inside that input. Reasoning tokens are part of the output.
func parseFxTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Read      *int64 `json:"cacheReadTokens"`
		Write     *int64 `json:"cacheWriteTokens"`
		Reasoning *int64 `json:"reasoningTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return Usage{}
	}
	return Usage{Usage: tokens.CacheInclusive(tokens.Report{Input: c.Input, Output: c.Output, CacheRead: c.Read, CacheWrite: c.Write, Reasoning: c.Reasoning})}
}
