package session

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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

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

// fxHome is the only home an fx session can use: the profile fx derives from
// the user's home directory. fx has no variable that moves it, so a caller
// that names another home is refused rather than silently given this one.
func fxHome(o Options) (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return o, refuse(o, "home", RefusedHome, "home directory unavailable")
	}
	fixed := filepath.Join(home, ".fx")
	cli := &o.Provider.CLI
	if cli.Home != "" {
		if cli.Home, err = filepath.Abs(cli.Home); err != nil || filepath.Clean(cli.Home) != fixed {
			return o, refuse(o, "home", RefusedHome, "fx reads its configuration and login from the user's home directory, and no variable moves it; leave Provider.CLI.Home empty")
		}
	}
	cli.Home = fixed
	return o, nil
}

// fxInitializeParams opens the protocol.
func fxInitializeParams() map[string]any {
	return map[string]any{"protocolVersion": fxProtocolVersion, "clientCapabilities": map[string]any{}}
}

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

// unsupportedFx refuses what fx has no way to carry, so that nothing a
// caller asked for is silently dropped. fx appends its session instructions to
// its own system prompt, so a request to replace that prompt is refused
// rather than answered with an append.
func unsupportedFx(o Options) error {
	engine := harness.Fx
	if o.Instructions.Mode == Replace {
		c := harness.Support(engine, harness.Session, harness.ReplaceInstructions)
		return &UnsupportedError{Engine: engine, Operation: "instructions", Code: RefusedNotOffered, Capability: c}
	}
	return nil
}

// fxForeignPolicy refuses the policy fields only the other engines read.
func fxForeignPolicy(o Options) error {
	p := o.Policy
	if p.CodexSandbox != "" || p.CodexApproval != "" || p.ClaudePermission != "" || p.ClaudeTools != nil || grokPolicySet(p) {
		return refuse(o, "policy", RefusedOtherEnginePolicy, "fx does not read the Codex, Claude or Grok policy fields; leave them unset")
	}
	if p.CommandCodePermission != "" || p.AntigravityPermission != "" {
		return refuse(o, "policy", RefusedOtherEnginePolicy, "fx does not read the Command Code or Antigravity policy fields; leave them unset")
	}
	return nil
}

// fxConfig is a session's configuration as its configOptions state it: the
// model, the effort, the mode, and the levels the effort accepts.
type fxConfig struct {
	model, effort, mode string
	efforts             []string
}

// parseFxConfig reads the configuration options. The effort's own levels are
// kept, because fx drops the effort option entirely for a model that does
// not offer one.
func parseFxConfig(raw json.RawMessage) (fxConfig, bool) {
	var options []struct {
		ID      string          `json:"id"`
		Current json.RawMessage `json:"currentValue"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if json.Unmarshal(raw, &options) != nil {
		return fxConfig{}, false
	}
	var c fxConfig
	for _, option := range options {
		switch option.ID {
		case "model":
			_ = json.Unmarshal(option.Current, &c.model)
		case "effort":
			_ = json.Unmarshal(option.Current, &c.effort)
			for _, value := range option.Options {
				c.efforts = append(c.efforts, value.Value)
			}
		case "mode":
			_ = json.Unmarshal(option.Current, &c.mode)
		}
	}
	return c, true
}

// fxSessionState is what session/new reported. A resumed session answers with
// its configuration alone, so the id and the modes may be absent.
type fxSessionState struct {
	id     string
	modes  []string
	config fxConfig
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
	config, ok := parseFxConfig(r.Config)
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
	body, err := s.transport.request(ctx, "initialize", fxInitializeParams())
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
		if resume && fxSessionGone(err) {
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
			s.closeFxSession(id)
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
	s.fxModel = config.model
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
func (s *Session) setFxMode(ctx context.Context, id string, state fxSessionState) (fxConfig, error) {
	if len(state.modes) > 0 && !slices.Contains(state.modes, fxAskingMode) {
		// The session named the modes it can run in, and the asking one is
		// not among them: there is no mode in which fx asks.
		return fxConfig{}, &UnsupportedError{Engine: harness.Fx, Operation: "permission mode", Code: RefusedNotOffered, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed fx does not offer a mode that asks before every change"}}
	}
	if state.config.mode == fxAskingMode {
		return state.config, nil
	}
	next, err := s.setFxOptionRaw(ctx, id, "mode", fxAskingMode)
	if err != nil {
		var refusal *acpRefusal
		if errors.As(err, &refusal) && refusal.code == fxInvalidParams {
			return fxConfig{}, &UnsupportedError{Engine: harness.Fx, Operation: "permission mode", Code: RefusedNotOffered, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed fx does not report the session's permission mode"}}
		}
		return fxConfig{}, fxConfigRefused(err, CapabilityChangedPermissionMode)
	}
	if next.mode != fxAskingMode {
		return fxConfig{}, &CapabilityError{Engine: harness.Fx, Code: CapabilityChangedPermissionMode, Phase: BeforeFirstPrompt}
	}
	return next, nil
}

// setFxOption sets one configuration option and returns the configuration fx
// then reports.
func (s *Session) setFxOption(ctx context.Context, id, option, value, code string) (fxConfig, error) {
	config, err := s.setFxOptionRaw(ctx, id, option, value)
	if err != nil {
		return fxConfig{}, fxConfigRefused(err, code)
	}
	return config, nil
}

// setFxOptionRaw sets one configuration option and returns what fx reported,
// with fx's own refusal unaltered, for the caller that reads it.
func (s *Session) setFxOptionRaw(ctx context.Context, id, option, value string) (fxConfig, error) {
	body, err := s.transport.request(ctx, "session/set_config_option", map[string]any{"sessionId": id, "configId": option, "value": value})
	if err != nil {
		return fxConfig{}, err
	}
	var r struct {
		Config json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(body, &r) != nil {
		return fxConfig{}, ErrProtocol
	}
	config, ok := parseFxConfig(r.Config)
	if !ok {
		return fxConfig{}, ErrProtocol
	}
	return config, nil
}

// fxConfigRefused turns fx's refusal of a setting into the capability failure
// it means. Anything else, such as a lost transport, is returned as it is.
func fxConfigRefused(err error, code string) error {
	var refusal *acpRefusal
	if errors.As(err, &refusal) && (refusal.code == fxInvalidParams || refusal.code == acpMethodMissing) {
		return &CapabilityError{Engine: harness.Fx, Code: code, Phase: BeforeFirstPrompt}
	}
	return err
}

// closeFxSession closes a session this launch created and then refused
// before any prompt, so a refused configuration leaves no empty conversation
// behind. It is best effort: the session is closing.
func (s *Session) closeFxSession(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.transport.request(ctx, "session/close", map[string]any{"sessionId": id})
}

// fxDialect is fx's agent protocol, answering permission requests by the
// session's policy.
func fxDialect(p Policy) dialect {
	return dialect{
		envelope:   acpEnvelope,
		parseReply: parseFxReply,
		answer: func(m map[string]json.RawMessage) (map[string]any, bool) {
			if !jsonRPCRequest(m) {
				return nil, false
			}
			return fxServerReply(m, p.FxPermission), true
		},
	}
}

// fxServerReply answers a request from the agent. Only a permission request
// is answered, by the session's policy and never with an "always" option,
// which would grant the tool for the rest of the session. Anything else is
// refused with a JSON-RPC error rather than left unanswered.
func fxServerReply(m map[string]json.RawMessage, permission string) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m["id"]}
	if str(m, "method") != "session/request_permission" {
		reply["error"] = refusedOperation()
		return reply
	}
	reply["result"] = map[string]any{"outcome": fxPermissionOutcome(m["params"], permission)}
	return reply
}

// fxPermissionOutcome selects the allow-once option under FxAllowWhenAsked
// and the reject-once option otherwise, and only when the request offers
// both. A request that offers neither, or that offers two options of the
// same kind or the same id, is answered cancelled: a shared id would let a
// deny be read as an allow. A tool that would change the permission mode is
// rejected under either policy, because allowing it would let every later
// change run without asking.
func fxPermissionOutcome(raw json.RawMessage, permission string) map[string]any {
	var p struct {
		ToolCall struct {
			Kind string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	cancelled := map[string]any{"outcome": "cancelled"}
	if json.Unmarshal(raw, &p) != nil {
		return cancelled
	}
	chosen := map[string]string{}
	ids := map[string]bool{}
	for _, option := range p.Options {
		if option.ID == "" {
			continue
		}
		if _, seen := chosen[option.Kind]; seen {
			return cancelled
		}
		if ids[option.ID] {
			return cancelled
		}
		chosen[option.Kind] = option.ID
		ids[option.ID] = true
	}
	allow, reject := chosen["allow_once"], chosen["reject_once"]
	if allow == "" || reject == "" {
		return cancelled
	}
	if permission == FxAllowWhenAsked && p.ToolCall.Kind != "switch_mode" {
		return map[string]any{"outcome": "selected", "optionId": allow}
	}
	return map[string]any{"outcome": "selected", "optionId": reject}
}

// fx's own refusal codes, beside the ones every ACP agent shares.
const (
	fxInvalidParams   = "invalid_params"
	fxSessionNotFound = "session_not_found"
)

// parseFxReply reads a JSON-RPC response as the other ACP agents' does, then
// classifies an error reply from what fx states in fixed fields.
func parseFxReply(m map[string]json.RawMessage) (string, response, bool, error) {
	id, r, isReply, err := parseCodexReply(m)
	if !isReply || err != nil || r.err == nil {
		return id, r, isReply, err
	}
	var e struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(m["error"], &e) != nil {
		return "", response{}, true, ErrProtocol
	}
	return id, response{err: &acpRefusal{engine: harness.Fx, rpc: e.Code, code: fxRefusalCode(e.Code, e.Message, e.Data)}}, true, nil
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
			return fxSessionNotFound
		}
		return fxInvalidParams
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

// fxSessionGone reports whether a refusal means the conversation is gone.
func fxSessionGone(err error) bool {
	var refusal *acpRefusal
	return errors.As(err, &refusal) && refusal.code == fxSessionNotFound
}
