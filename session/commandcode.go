package session

// Command Code's Agent Client Protocol: JSON-RPC 2.0 over the stdio of
// `cmd acp`. Every method and shape here was checked against Command Code
// 1.74.1:
//
//   - `cmd acp` takes no options. It reuses the login `cmd login` stored under
//     the user's home directory (authMethods "command-code-cli"), which no
//     variable relocates.
//   - initialize {protocolVersion: 1, clientCapabilities: {}} answers with
//     protocolVersion 1 and agentCapabilities, whose sessionCapabilities
//     advertise list, resume and close.
//   - session/new {cwd, mcpServers: []} answers with the sessionId, the modes
//     and the configOptions "model" and, for a model that has one, "effort".
//     It reads no other field, so there is nowhere to send instructions.
//   - session/resume {sessionId, cwd, mcpServers} succeeds for any id: one
//     Command Code does not have is opened as a new, empty conversation under
//     that id. session/list {cwd} lists the conversations it holds for a
//     directory, so a resume is checked against it before and after.
//   - session/set_config_option {sessionId, configId, value} sets the model or
//     the effort and answers with the resulting configOptions. An unknown value
//     is refused with -32602; an effort for a model without one with -32601.
//   - session/set_mode {sessionId, modeId} selects a permission mode. Mode
//     "default" (Standard) asks before every tool that changes anything. Every
//     mode change, set_mode's own included, is announced as a
//     current_mode_update before set_mode answers; the agent can also change
//     mode itself, through tools of kind "switch_mode".
//   - session/prompt streams session/update notifications and answers, when
//     the turn is over, with its stopReason, the session's cumulative usage,
//     and the turn's own usage in _meta.usage. Only one prompt runs at a time.
//   - session/cancel is a notification; the running prompt then answers with
//     stopReason "cancelled".
//   - session/request_permission is a request from the agent. A permission
//     request offers allow_once and reject_once (and the "always" options
//     unless the call carries a risk). Command Code also asks the user a
//     question through it, offering one allow_once option per answer.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"

	harness "github.com/shhac/lib-agent-harness"
)

// commandCodeProtocolVersion is the only Agent Client Protocol version spoken
// to Command Code.
const commandCodeProtocolVersion = 1

// commandCodeAskingMode is the permission mode every session runs in. Tools
// allowed by configuration can still run without asking.
const commandCodeAskingMode = "default"

// commandCodeListPages bounds how far session/list is followed when looking
// for a conversation to resume.
const commandCodeListPages = 50

func commandCodeArgs() []string { return []string{"acp"} }

func normalizeCommandCodePolicy(o Options) (Options, error) {
	switch o.Policy.CommandCodePermission {
	case "":
		o.Policy.CommandCodePermission = CommandCodeDenyWhenAsked
	case CommandCodeDenyWhenAsked, CommandCodeAllowWhenAsked:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Command Code permission policy")
	}
	return o, nil
}

func commandCodeSessionParams(o Options, resume bool, id string) map[string]any {
	p := map[string]any{"cwd": o.WorkDir, "mcpServers": []any{}}
	if resume {
		p["sessionId"] = id
	}
	return p
}

// commandCodeSessionState is what session/new or session/resume reported.
type commandCodeSessionState struct {
	id, mode string
	modes    []string
	config   acpConfig
}

func parseCommandCodeSession(raw json.RawMessage) (commandCodeSessionState, error) {
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
	if json.Unmarshal(raw, &r) != nil || r.Modes == nil {
		return commandCodeSessionState{}, ErrProtocol
	}
	config, ok := parseACPConfig(r.Config)
	if !ok || config.model == "" {
		return commandCodeSessionState{}, ErrProtocol
	}
	state := commandCodeSessionState{id: r.SessionID, mode: r.Modes.Current, config: config}
	for _, mode := range r.Modes.Available {
		state.modes = append(state.modes, mode.ID)
	}
	return state, nil
}

// initializeCommandCode opens the protocol, creates or resumes the session,
// puts it in the asking mode and applies the model and effort, checking each
// against what the session then reports.
func (s *Session) initializeCommandCode(ctx context.Context, resume bool) error {
	body, err := s.transport.request(ctx, "initialize", acpInitializeParams(commandCodeProtocolVersion))
	if err != nil {
		return err
	}
	agent, err := parseACPAgent(body, commandCodeProtocolVersion)
	if err != nil {
		return err
	}
	if resume && (!agent.resume || !agent.list) {
		return &UnsupportedError{Engine: harness.CommandCode, Operation: "resume", Code: RefusedMethodMissing, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed Command Code does not offer session/resume and session/list"}}
	}
	s.mu.Lock()
	id := s.ref.ID
	s.mu.Unlock()
	method := "session/new"
	if resume {
		held, err := s.commandCodeHolds(ctx, id)
		if err != nil {
			return err
		}
		if !held {
			return errConversationGone
		}
		method = "session/resume"
	}
	body, err = s.transport.request(ctx, method, commandCodeSessionParams(s.options, resume, id))
	if err != nil {
		return err
	}
	if resume {
		held, err := s.commandCodeHolds(ctx, id)
		if err != nil {
			return err
		}
		if !held {
			s.closeACPSession(id)
			return errConversationGone
		}
	}
	state, err := parseCommandCodeSession(body)
	if err != nil {
		return err
	}
	if !resume {
		if state.id == "" {
			return ErrProtocol
		}
		id = state.id
	}
	if err = s.configureCommandCode(ctx, id, state); err != nil {
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

// commandCodeHolds reports whether session/list names the conversation for
// the session's working directory.
func (s *Session) commandCodeHolds(ctx context.Context, id string) (bool, error) {
	params := map[string]any{"cwd": s.options.WorkDir}
	for range commandCodeListPages {
		body, err := s.transport.request(ctx, "session/list", params)
		if err != nil {
			return false, err
		}
		var page struct {
			Sessions *[]struct {
				ID  string `json:"sessionId"`
				Cwd string `json:"cwd"`
			} `json:"sessions"`
			Next string `json:"nextCursor"`
		}
		if json.Unmarshal(body, &page) != nil || page.Sessions == nil {
			return false, ErrProtocol
		}
		for _, listed := range *page.Sessions {
			if listed.ID == id && sameDirectory(listed.Cwd, s.options.WorkDir) {
				return true, nil
			}
		}
		if page.Next == "" {
			return false, nil
		}
		params["cursor"] = page.Next
	}
	return false, ErrProtocol
}

// sameDirectory reports whether two paths name the same directory. Command
// Code may list the path it was given or the one it resolves to.
func sameDirectory(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

// configureCommandCode puts the session in the asking mode, then sets the
// requested model and effort. Command Code reports the configuration each
// change produced, and that report, not the request, is what is checked. From
// here on the session watches the mode Command Code reports (see
// commandCodeModeUpdate), and the mode is checked again once the rest is set.
func (s *Session) configureCommandCode(ctx context.Context, id string, state commandCodeSessionState) error {
	// Command Code announces a mode only when it changes, so set_mode to the
	// mode a session already runs in reports nothing. The reply's mode is
	// therefore the baseline, unless a later report reached the reader first.
	s.mu.Lock()
	mode := state.mode
	if s.commandCodeEarly.session == id {
		mode = s.commandCodeEarly.mode
	}
	s.commandCodeWatch, s.commandCodeMode, s.commandCodeEarly.session = id, mode, ""
	s.mu.Unlock()
	if !slices.Contains(state.modes, commandCodeAskingMode) {
		return &CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedPermissionMode, Phase: BeforeFirstPrompt}
	}
	if _, err := s.transport.request(ctx, "session/set_mode", map[string]any{"sessionId": id, "modeId": commandCodeAskingMode}); err != nil {
		return acpConfigRefused(harness.CommandCode, err, CapabilityChangedPermissionMode)
	}
	config := state.config
	o := s.options
	if o.Model != "" && config.model != o.Model {
		next, err := s.setCommandCodeOption(ctx, id, "model", o.Model, CapabilityChangedModel)
		if err != nil {
			return err
		}
		if next.model != o.Model {
			return &CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedModel, Phase: BeforeFirstPrompt}
		}
		config = next
	}
	if o.Effort != "" && config.effort != o.Effort {
		if !slices.Contains(config.efforts, o.Effort) {
			return &CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedEffort, Phase: BeforeFirstPrompt}
		}
		next, err := s.setCommandCodeOption(ctx, id, "effort", o.Effort, CapabilityChangedEffort)
		if err != nil {
			return err
		}
		if next.effort != o.Effort || (o.Model != "" && next.model != o.Model) {
			return &CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedEffort, Phase: BeforeFirstPrompt}
		}
		config = next
	}
	// The mode update set_mode announces arrives before its answer, so by now
	// the reported mode is what the session runs in.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commandCodeMode != commandCodeAskingMode {
		return &CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedPermissionMode, Phase: BeforeFirstPrompt}
	}
	s.acpModel, s.commandCodeConfigured = config.model, true
	return nil
}

// commandCodeModeUpdate records a mode Command Code reports for the session
// being configured or run, wherever it arrives: in a turn, between turns or
// during configuration. Once the session is configured, any mode but the
// asking one stops it, because every change its tools make would then run
// without reaching the session's policy.
func (s *Session) commandCodeModeUpdate(m map[string]json.RawMessage) bool {
	if str(m, "method") != "session/update" {
		return false
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind string `json:"sessionUpdate"`
			Mode string `json:"currentModeId"`
		} `json:"update"`
	}
	if json.Unmarshal(m["params"], &p) != nil || p.Update.Kind != "current_mode_update" {
		return false
	}
	s.mu.Lock()
	ours := p.SessionID != "" && p.SessionID == s.commandCodeWatch
	if ours {
		s.commandCodeMode = p.Update.Mode
	} else if p.SessionID != "" && !s.commandCodeConfigured {
		s.commandCodeEarly.session, s.commandCodeEarly.mode = p.SessionID, p.Update.Mode
	}
	stop := ours && s.commandCodeConfigured && p.Update.Mode != commandCodeAskingMode
	s.mu.Unlock()
	if stop {
		s.fail(&CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedPermissionMode, Phase: DuringSession})
	}
	return true
}

func (s *Session) setCommandCodeOption(ctx context.Context, id, option, value, code string) (acpConfig, error) {
	config, err := s.setACPOption(ctx, id, option, value)
	if err != nil {
		return acpConfig{}, acpConfigRefused(harness.CommandCode, err, code)
	}
	if config.model == "" {
		return acpConfig{}, ErrProtocol
	}
	return config, nil
}

// commandCodeDialect is Command Code's ACP, answering permission requests by
// the session's policy with the strict once-only answer.
func commandCodeDialect(p Policy) dialect {
	return acpDialect(harness.CommandCode, commandCodeRefusalCode, func(params json.RawMessage) map[string]any {
		return acpStrictPermissionOutcome(params, p.CommandCodePermission == CommandCodeAllowWhenAsked)
	})
}

// commandCodeBusy is Command Code's own refusal code, beside the ones ACP
// agents share.
const commandCodeBusy = "prompt_running"

// commandCodeRefusalCode classifies by the JSON-RPC code and, for a failed
// run, the provider's HTTP status that Command Code passes on in data.
func commandCodeRefusalCode(code int, _ string, data json.RawMessage) string {
	switch code {
	case -32601:
		return acpMethodMissing
	case -32602:
		return acpInvalidParams
	case -32600:
		return commandCodeBusy
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
