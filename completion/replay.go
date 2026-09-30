package completion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Some reasoning endpoints return state beside an assistant message that they
// need back with it on the next request: DeepSeek and Kimi thinking modes
// refuse a tool-call history without its reasoning_content, OpenRouter asks
// for reasoning_details, and Gemini's OpenAI endpoint signs each tool call's
// thought in extra_content. Dropping it breaks a multi-step tool loop.
//
// The library returns it as Message.Replay and ToolCall.Replay, opaque to the
// caller, and sends it back only to the endpoint and model that produced it:
// another provider could not use it, and should not be handed one provider's
// reasoning. Only the fields named here are carried.
var (
	messageReplayFields = []string{"reasoning_content", "reasoning_details"}
	callReplayFields    = []string{"extra_content"}
)

// replay is the envelope a Replay value holds.
type replay struct {
	For    string                     `json:"for"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// replayBinding names the endpoint and model a replay belongs to. It is a
// digest, so the value carries no URL a caller might log.
func replayBinding(cfg Config) string {
	sum := sha256.Sum256([]byte(cfg.Provider.API.BaseURL + "\x00" + cfg.Model))
	return hex.EncodeToString(sum[:16])
}

// captureReplay keeps the named fields a response carried, or nil when it
// carried none.
func captureReplay(binding string, raw map[string]json.RawMessage, names []string) json.RawMessage {
	fields := map[string]json.RawMessage{}
	for _, name := range names {
		if value := raw[name]; len(value) > 0 && string(value) != "null" {
			fields[name] = value
		}
	}
	if len(fields) == 0 {
		return nil
	}
	out, _ := json.Marshal(replay{For: binding, Fields: fields})
	return out
}

// openReplay returns the fields a Replay carries for this binding. False
// means it belongs elsewhere or is not one this library made.
func openReplay(value json.RawMessage, binding string, names []string) (map[string]json.RawMessage, bool) {
	if len(value) == 0 {
		return nil, true
	}
	var r replay
	if json.Unmarshal(value, &r) != nil || r.For != binding || len(r.Fields) == 0 {
		return nil, false
	}
	for name := range r.Fields {
		allowed := false
		for _, known := range names {
			allowed = allowed || name == known
		}
		if !allowed {
			return nil, false
		}
	}
	return r.Fields, true
}

// carriesReplay reports whether any message or call holds provider state.
func carriesReplay(messages []Message) bool {
	for _, message := range messages {
		if len(message.Replay) > 0 {
			return true
		}
		for _, call := range message.ToolCalls {
			if len(call.Replay) > 0 {
				return true
			}
		}
	}
	return false
}
