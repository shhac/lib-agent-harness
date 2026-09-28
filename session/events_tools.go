package session

// Tool activity: what a tool was asked to do and what it produced, carried on
// tool_started and tool_completed events. Each engine's dialect extracts the
// payload; this file bounds it, identically for every engine.
//
// A hosted call in a restricted or sandboxed CLI session is reported once, by
// the engine's own events (a Codex mcpToolCall item, a Claude mcp__ tool_use),
// which carry its arguments and the result the handler returned. The tool
// channel emits no events of its own. An API session has no engine, so its
// loop reports each call itself.

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// MaxToolPayloadBytes bounds Event.Input and Event.Output, each, on one
// event. A command's whole output or a large file write can run to megabytes,
// and an event stream is for following activity, not for archiving it.
const MaxToolPayloadBytes = 64 << 10

// withToolPayload sets an event's tool input and output, bounded, and with the
// library's own tool-channel credential removed should a tool have read it.
// Everything else in a payload is the application's data and passes through.
func (s *Session) withToolPayload(e Event, input json.RawMessage, output string) Event {
	secret := s.channelSecret()
	if !jsonAbsent(input) {
		e.Input, e.InputTruncated = boundToolInput(scrub(input, secret), MaxToolPayloadBytes)
	}
	if output != "" {
		e.Output, e.OutputTruncated = boundToolOutput(string(scrub([]byte(output), secret)), MaxToolPayloadBytes)
	}
	return e
}

// channelSecret is the hosted tool channel's credential, when this session has
// one. A shell in a sandboxed session can read the file that holds it.
func (s *Session) channelSecret() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tools == nil {
		return nil
	}
	return s.tools.secret
}

// scrub replaces the credential with a marker. The credential is hex, so the
// replacement never breaks the JSON text it appears in.
func scrub(raw, secret []byte) []byte {
	if len(secret) == 0 || !strings.Contains(string(raw), string(secret)) {
		return raw
	}
	return []byte(strings.ReplaceAll(string(raw), string(secret), "[redacted]"))
}

// boundToolInput keeps arguments that fit as they are. Text that is not JSON
// — a model's malformed arguments — is carried as a JSON string, and so is the
// head of arguments too large to keep, with the encoded string bounded too.
func boundToolInput(raw []byte, limit int) (json.RawMessage, bool) {
	if len(raw) <= limit && json.Valid(raw) {
		return append(json.RawMessage(nil), raw...), false
	}
	truncated := len(raw) > limit
	head := cutRunes(string(raw), limit)
	for {
		encoded, _ := json.Marshal(head)
		if len(encoded) <= limit {
			return encoded, truncated
		}
		head = cutRunes(head, len(head)-(len(encoded)-limit))
		truncated = true
	}
}

func boundToolOutput(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	return cutRunes(text, limit), true
}

// cutRunes returns at most limit bytes of text without splitting a character.
func cutRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// jsonAbsent reports a value the harness did not supply.
func jsonAbsent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

// toolResultText reads a tool result's content the way both MCP and Claude
// shape it: a plain string, or blocks whose text parts are the result. Content
// with no text in it is kept as its JSON text rather than dropped.
func toolResultText(raw json.RawMessage) string {
	if jsonAbsent(raw) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		if len(blocks) == 0 {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}

// pickFields builds an input object from the named fields a harness reported,
// omitting those it left out, for items whose arguments are spread across the
// item rather than held in one field.
func pickFields(item map[string]json.RawMessage, keys ...string) json.RawMessage {
	picked := map[string]json.RawMessage{}
	for _, key := range keys {
		if !jsonAbsent(item[key]) {
			picked[key] = item[key]
		}
	}
	if len(picked) == 0 {
		return nil
	}
	raw, err := json.Marshal(picked)
	if err != nil {
		return nil
	}
	return raw
}
