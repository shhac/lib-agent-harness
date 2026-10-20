package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// The configuration Command Code and fx state as a session's configOptions,
// and set with session/set_config_option. What each engine requires of it,
// and which options it sets, are its own.

// acpInitializeParams opens the protocol at the only version spoken.
func acpInitializeParams(version int) map[string]any {
	return map[string]any{"protocolVersion": version, "clientCapabilities": map[string]any{}}
}

// acpConfig is a session's configuration as its configOptions state it: the
// model, the effort, the mode, and the levels the effort accepts.
type acpConfig struct {
	model, effort, mode string
	efforts             []string
}

// parseACPConfig reads the configuration options, reporting false only when
// they cannot be read. The effort's own levels are kept, because an agent may
// drop the effort option entirely for a model that does not offer one.
func parseACPConfig(raw json.RawMessage) (acpConfig, bool) {
	var options []struct {
		ID      string          `json:"id"`
		Current json.RawMessage `json:"currentValue"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if json.Unmarshal(raw, &options) != nil {
		return acpConfig{}, false
	}
	var c acpConfig
	for _, option := range options {
		switch option.ID {
		case "model":
			_ = json.Unmarshal(option.Current, &c.model)
		case "effort":
			if json.Unmarshal(option.Current, &c.effort) != nil {
				c.effort = ""
			}
			for _, value := range option.Options {
				c.efforts = append(c.efforts, value.Value)
			}
		case "mode":
			_ = json.Unmarshal(option.Current, &c.mode)
		}
	}
	return c, true
}

// setACPOption sets one configuration option and returns the configuration
// the agent then reports, with the agent's own refusal unaltered.
func (s *Session) setACPOption(ctx context.Context, id, option, value string) (acpConfig, error) {
	body, err := s.transport.request(ctx, "session/set_config_option", map[string]any{"sessionId": id, "configId": option, "value": value})
	if err != nil {
		return acpConfig{}, err
	}
	var r struct {
		Config json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(body, &r) != nil {
		return acpConfig{}, ErrProtocol
	}
	config, ok := parseACPConfig(r.Config)
	if !ok {
		return acpConfig{}, ErrProtocol
	}
	return config, nil
}

// acpConfigRefused turns an agent's refusal of a setting into the capability
// failure it means. Anything else, such as a lost transport, is returned as
// it is.
func acpConfigRefused(engine harness.Engine, err error, code string) error {
	var refusal *acpRefusal
	if errors.As(err, &refusal) && (refusal.code == acpInvalidParams || refusal.code == acpMethodMissing) {
		return &CapabilityError{Engine: engine, Code: code, Phase: BeforeFirstPrompt}
	}
	return err
}

// closeACPSession closes a session this launch created, or resumed as empty,
// and then refused before any prompt, so a refused configuration leaves no
// empty conversation behind. It is best effort: the session is closing.
func (s *Session) closeACPSession(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.transport.request(ctx, "session/close", map[string]any{"sessionId": id})
}
