package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
)

// fx's Agent Client Protocol: JSON-RPC 2.0 over the stdio of `fx acp`. Every
// method and shape here was checked against fx 0.0.14:
//
//   - initialize {protocolVersion: 1, clientCapabilities: {}} answers with
//     protocolVersion 1, agentCapabilities, whose sessionCapabilities offer
//     list, resume and close, and agentInfo.
//   - session/new {cwd, mcpServers: []} answers with the sessionId, the
//     session's configOptions and the modes it can run in. Instructions travel
//     as systemPrompt content blocks, which fx appends to its own system
//     prompt; there is no way to replace that prompt.
//   - session/resume {sessionId, cwd, mcpServers} answers with the
//     configOptions and the modes, but no session id, and with -32602
//     "Session not found" for a conversation fx does not hold. The
//     instructions the session was created with still apply.
//   - session/set_config_option {sessionId, configId, value} sets the
//     provider, the model, the mode, the effort or the ultrafast tier, and
//     answers with the resulting configOptions. The mode is one of them, so a
//     mode change is checked against the session's own report, and so is a
//     model or an effort. An unknown option, an effort the active model does
//     not offer and an unknown mode are all refused with -32602.
//   - session/set_mode {sessionId, modeId} answers with null and reports
//     nothing, so it is not used: a mode set that cannot be read back is a
//     permission the session cannot account for.
//   - session/prompt streams session/update notifications and answers, when
//     the turn is over, with its stopReason and the turn's own usage.
//   - session/cancel is a notification; the running prompt then answers with
//     stopReason "cancelled".
//   - session/request_permission is a request from the agent, offering
//     allow_once, allow_always and reject_once. A request that offers no
//     option the session's policy wants is answered cancelled, so neither a
//     permission nor an answer is ever guessed.

// fxProtocolVersion is the only Agent Client Protocol version spoken to fx.
const fxProtocolVersion = 1

// fxAskingMode is the mode every session runs in: fx asks before every action
// that changes anything, so the session's policy decides what is allowed.
// The mode is one of the session's config options, so what the session runs
// in is read back from fx rather than assumed.
const fxAskingMode = "ask"

// fxArgs starts fx's agent protocol on stdio. fx takes its global workspace
// flags before the subcommand, and this session needs none of them: the
// working directory travels with session/new, and the model, the effort and
// the mode are set as the session's own configuration.
func fxArgs() []string { return []string{"acp"} }

// fxSessionParams creates, or resumes by id, a session in the working
// directory. No MCP server is added: fx loads the project's own, and the
// library hosts no tools for it.
//
// Instructions are sent only when the session is created: fx restores them
// with the session it saved, and sending them again would append them twice.
func fxSessionParams(o Options, resume bool, id string) map[string]any {
	p := map[string]any{"cwd": o.WorkDir, "mcpServers": []any{}}
	if resume {
		p["sessionId"] = id
		return p
	}
	if instructions := effectiveInstructions(o); instructions.Mode == Append && instructions.Text != "" {
		p["systemPrompt"] = []any{map[string]any{"type": "text", "text": instructions.Text}}
	}
	return p
}

// normalizeFxPolicy applies fx's permission policy. fx runs in the asking
// mode, so every action that changes anything reaches the session's policy;
// an unset policy rejects each request, which is the safe answer.
func normalizeFxPolicy(o Options) (Options, error) {
	switch o.Policy.FxPermission {
	case "":
		o.Policy.FxPermission = FxDenyWhenAsked
	case FxDenyWhenAsked, FxAllowWhenAsked:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid fx permission policy")
	}
	return o, nil
}

// fxSessionState is what session/new reported. A resumed session answers with
// its configuration alone, so the id and the modes may be absent.
type fxSessionState struct {
	id     string
	modes  []string
	config acpConfig
}

// parseFxSession reads the session's own report.
func parseFxSession(raw json.RawMessage) (fxSessionState, error) {
	var r struct {
		SessionID string `json:"sessionId"`
		Modes     *struct {
			Current   string `json:"currentModeId"`
			Available []struct {
				ID string `json:"id"`
			} `json:"availableModes"`
		} `json:"modes"`
		Config json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return fxSessionState{}, ErrProtocol
	}
	config, ok := parseACPConfig(r.Config)
	if !ok {
		return fxSessionState{}, ErrProtocol
	}
	state := fxSessionState{id: r.SessionID, config: config}
	if r.Modes != nil {
		for _, mode := range r.Modes.Available {
			state.modes = append(state.modes, mode.ID)
		}
	}
	return state, nil
}

// initializeFx opens the protocol, creates or resumes the session, and puts
// it in the asking mode with the model and effort the caller asked for,
// checking each against what the session then reports.
func (s *Session) initializeFx(ctx context.Context, resume bool) error {
	body, err := s.transport.request(ctx, "initialize", acpInitializeParams(fxProtocolVersion))
	if err != nil {
		return err
	}
	agent, err := parseACPAgent(body, fxProtocolVersion)
	if err != nil {
		return err
	}
	if resume && !agent.resume {
		return &UnsupportedError{Engine: harness.Fx, Operation: "resume", Code: RefusedMethodMissing, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed fx does not offer session/resume"}}
	}
	s.mu.Lock()
	id := s.ref.ID
	s.mu.Unlock()
	method := "session/new"
	if resume {
		method = "session/resume"
	}
	body, err = s.transport.request(ctx, method, fxSessionParams(s.options, resume, id))
	if err != nil {
		if resume && acpSessionGone(err) {
			return fmt.Errorf("%w: %w", errConversationGone, err)
		}
		return err
	}
	state, err := parseFxSession(body)
	if err != nil {
		return err
	}
	if !resume {
		if state.id == "" {
			return ErrProtocol
		}
		id = state.id
	}
	if err = s.configureFx(ctx, id, state); err != nil {
		if !resume {
			s.closeACPSession(id)
		}
		return err
	}
	s.mu.Lock()
	s.ref.ID = id
	s.mu.Unlock()
	return nil
}

// configureFx puts the session in the asking mode, then sets the model and
// the effort. Each change is read back from fx, and the session's own report
// is what is checked rather than the request's success. fx 0.0.14 refuses a
// model or an effort it does not offer with -32602.
func (s *Session) configureFx(ctx context.Context, id string, state fxSessionState) error {
	config, err := s.setFxMode(ctx, id, state)
	if err != nil {
		return err
	}
	o := s.options
	if o.Model != "" && config.model != o.Model {
		next, err := s.setFxOption(ctx, id, "model", o.Model, CapabilityChangedModel)
		if err != nil {
			return err
		}
		if next.model != o.Model {
			return &CapabilityError{Engine: harness.Fx, Code: CapabilityChangedModel, Phase: BeforeFirstPrompt}
		}
		config = next
	}
	if o.Effort != "" && config.effort != o.Effort {
		next, err := s.setFxOption(ctx, id, "effort", o.Effort, CapabilityChangedEffort)
		if err != nil {
			return err
		}
		if next.effort != o.Effort {
			return &CapabilityError{Engine: harness.Fx, Code: CapabilityChangedEffort, Phase: BeforeFirstPrompt}
		}
		config = next
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acpModel = config.model
	return nil
}

// setFxMode puts the session in the asking mode. The mode is set as a
// configuration option, because that is the only way fx reports the mode it
// then runs in: session/set_mode answers with null and nothing else, so a
// mode set that way could not be checked, and an unchecked permission mode is
// not one this library will run with.
//
// fx announces no mode change over the protocol, so a mode changed outside
// the session is not observed; the session's own report is the evidence there
// is.
func (s *Session) setFxMode(ctx context.Context, id string, state fxSessionState) (acpConfig, error) {
	if len(state.modes) > 0 && !slices.Contains(state.modes, fxAskingMode) {
		// The session named the modes it can run in, and the asking one is
		// not among them: there is no mode in which fx asks.
		return acpConfig{}, &UnsupportedError{Engine: harness.Fx, Operation: "permission mode", Code: RefusedNotOffered, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed fx does not offer a mode that asks before every change"}}
	}
	if state.config.mode == fxAskingMode {
		return state.config, nil
	}
	next, err := s.setACPOption(ctx, id, "mode", fxAskingMode)
	if err != nil {
		var refusal *acpRefusal
		if errors.As(err, &refusal) && refusal.code == acpInvalidParams {
			return acpConfig{}, &UnsupportedError{Engine: harness.Fx, Operation: "permission mode", Code: RefusedNotOffered, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed fx does not report the session's permission mode"}}
		}
		return acpConfig{}, acpConfigRefused(harness.Fx, err, CapabilityChangedPermissionMode)
	}
	if next.mode != fxAskingMode {
		return acpConfig{}, &CapabilityError{Engine: harness.Fx, Code: CapabilityChangedPermissionMode, Phase: BeforeFirstPrompt}
	}
	return next, nil
}

// setFxOption sets one configuration option and returns the configuration fx
// then reports.
func (s *Session) setFxOption(ctx context.Context, id, option, value, code string) (acpConfig, error) {
	config, err := s.setACPOption(ctx, id, option, value)
	if err != nil {
		return acpConfig{}, acpConfigRefused(harness.Fx, err, code)
	}
	return config, nil
}

// fxDialect is fx's agent protocol, answering permission requests by the
// session's policy with the strict once-only answer.
func fxDialect(p Policy) dialect {
	return acpDialect(harness.Fx, fxRefusalCode, func(params json.RawMessage) map[string]any {
		return acpStrictPermissionOutcome(params, p.FxPermission == FxAllowWhenAsked)
	})
}

// fxRefusalCode classifies by the JSON-RPC code, and reads fx's message only
// for the one distinction that matters: a conversation that is gone is a
// different failure from a request that was wrong. The message is read, never
// kept.
func fxRefusalCode(code int, message string, data json.RawMessage) string {
	switch code {
	case -32601:
		return acpMethodMissing
	case -32602:
		if strings.Contains(message, "Session not found") {
			return acpSessionNotFound
		}
		return acpInvalidParams
	case -32000:
		return acpAuthRequired
	}
	var d struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal(data, &d)
	if status := acpStatusCode(d.Status); status != "" {
		return status
	}
	return acpRejected
}

// fxManaged is what an fx session never inherits: fx's own variables, which
// change which sign-in it uses, which model it runs, how it asks and how it
// reports, except its protective switches.
func fxManaged(key string) bool {
	upper := strings.ToUpper(key)
	if fxProtected(upper) {
		return false
	}
	return strings.HasPrefix(upper, "FX_")
}

// fxProtected are the fx variables that carry the operator's own choice of a
// bound, a cost or a network switch. Dropping one would run the session with
// more room, at more cost, or reaching further than the operator allowed, so
// these are inherited and accepted as additions; the rest of fx's variables
// stay managed.
//
// fx has no environment variable that carries a privacy guarantee: its Zero
// Data Retention control is a Gateway account setting, which no environment
// variable reaches.
func fxProtected(upper string) bool {
	switch upper {
	case "FX_MAX_AGENT_STEPS", "FX_ULTRAFAST", "FX_AUTO_UPGRADE":
		return true
	}
	return false
}
