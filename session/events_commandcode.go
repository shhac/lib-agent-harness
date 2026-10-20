package session

import (
	"encoding/json"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/tokens"
)

// Command Code's session/update dialect, and the prompt response that ends a
// turn. Every shape here was observed from Command Code 1.74.1. Its prompt
// response carries two usage scopes: usage is the session's running total,
// and _meta.usage is this turn's own, which is the one reported.

// readCommandCodePromptEnd reads Command Code's prompt response, whose
// _meta.usage is the turn's own accounting.
func readCommandCodePromptEnd(body json.RawMessage, interruptRequested bool) (acpTurnEnd, bool) {
	var r struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"_meta"`
	}
	if json.Unmarshal(body, &r) != nil || r.StopReason == "" {
		return acpTurnEnd{}, false
	}
	status, code := acpOutcome(r.StopReason, interruptRequested)
	return acpTurnEnd{status: status, code: code, usage: parseCommandCodeTurnUsage(r.Meta.Usage)}, true
}

// commandCodeEvent routes one notification for the active turn.
func (s *Session) commandCodeEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if str(m, "method") == "session/update" {
		s.acpUpdate(t, ref, m["params"], commandCodeUpdates)
	}
}

// commandCodeUpdates starts a new message after each tool call, and reads
// Command Code's own estimate of the context.
var commandCodeUpdates = acpUpdates{
	messageItem: acpCountedItem("message-"), newMessageAfterTool: true, toolName: commandCodeToolName,
	context: func(s *Session, t *Turn, update map[string]json.RawMessage) {
		s.acpContext(t, update, harness.Estimated, "Command Code's own count of the conversation's context")
	},
}

// commandCodeToolKinds are the Agent Client Protocol's tool kinds.
var commandCodeToolKinds = map[string]bool{"read": true, "edit": true, "delete": true, "move": true, "search": true, "execute": true, "think": true, "fetch": true, "switch_mode": true, "other": true}

// commandCodeToolName is the tool's own identifier where its title is one,
// and otherwise its protocol kind. Command Code titles a known tool with a
// label and its path or command, which are never used as a name.
func commandCodeToolName(update map[string]json.RawMessage) string {
	if title := str(update, "title"); acpToolIdentifier.MatchString(title) {
		return title
	}
	if kind := str(update, "kind"); commandCodeToolKinds[kind] {
		return kind
	}
	return "tool"
}

// parseCommandCodeTurnUsage reads a turn's own accounting. Command Code sums
// each model response's totals, whose inputTokens already counts cached input
// (the cache figures are its details), so Input is taken as it is. A turn
// that reported nothing has no _meta.usage, and stays unknown.
func parseCommandCodeTurnUsage(raw json.RawMessage) Usage {
	var c struct {
		Input  *int64 `json:"inputTokens"`
		Output *int64 `json:"outputTokens"`
		Read   *int64 `json:"cacheReadTokens"`
		Write  *int64 `json:"cacheWriteTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return Usage{}
	}
	return Usage{Usage: tokens.CacheInclusive(tokens.Report{Input: c.Input, Output: c.Output, CacheRead: c.Read, CacheWrite: c.Write})}
}
