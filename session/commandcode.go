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
//     directory, so a resume is checked against it first.
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
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// commandCodeProtocolVersion is the only Agent Client Protocol version spoken
// to Command Code.
const commandCodeProtocolVersion = 1

// commandCodeAskingMode is the permission mode every session runs in, so that
// every change Command Code's tools would make reaches the session's policy.
const commandCodeAskingMode = "default"

// commandCodeListPages bounds how far session/list is followed when looking
// for a conversation to resume.
const commandCodeListPages = 50

func commandCodeArgs() []string { return []string{"acp"} }

// commandCodeHome is the only home a Command Code session can use: the one
// Command Code derives from the user's home directory. A caller that names
// another is refused rather than silently given this one.
func commandCodeHome(o Options) (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return o, refuse(o, "home", RefusedHome, "home directory unavailable")
	}
	fixed := filepath.Join(home, ".commandcode")
	cli := &o.Provider.CLI
	if cli.Home != "" {
		if cli.Home, err = filepath.Abs(cli.Home); err != nil || filepath.Clean(cli.Home) != fixed {
			return o, refuse(o, "home", RefusedHome, "Command Code reads its configuration and login from the user's home directory, and no variable moves it; leave Provider.CLI.Home empty")
		}
	}
	cli.Home = fixed
	return o, nil
}

// unsupportedCommandCode refuses what cmd acp has no way to carry, so that
// nothing a caller asked for is silently dropped.
func unsupportedCommandCode(o Options) error {
	engine := harness.CommandCode
	if o.Instructions.Mode != "" || o.Instructions.Text != "" {
		feature := harness.AppendInstructions
		if o.Instructions.Mode == Replace {
			feature = harness.ReplaceInstructions
		}
		if c := harness.Support(engine, harness.Session, feature); !c.Usable() {
			return &UnsupportedError{Engine: engine, Operation: "instructions", Code: RefusedNotOffered, Capability: c}
		}
	}
	if len(o.Skills.Provided) > 0 {
		if c := harness.Support(engine, harness.Session, harness.ProvidedSkills); !c.Usable() {
			return &UnsupportedError{Engine: engine, Operation: "skills", Code: RefusedNotOffered, Capability: c}
		}
	}
	return nil
}

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

// commandCodeAgent is what initialize established about the installed agent.
type commandCodeAgent struct{ resume, list bool }

func parseCommandCodeAgent(raw json.RawMessage) (commandCodeAgent, error) {
	var r struct {
		ProtocolVersion *int `json:"protocolVersion"`
		Capabilities    struct {
			Session struct {
				Resume json.RawMessage `json:"resume"`
				List   json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ProtocolVersion == nil || *r.ProtocolVersion != commandCodeProtocolVersion {
		return commandCodeAgent{}, ErrProtocol
	}
	return commandCodeAgent{resume: !rawjson.Absent(r.Capabilities.Session.Resume), list: !rawjson.Absent(r.Capabilities.Session.List)}, nil
}

// commandCodeConfig is a session's model and effort as its configOptions
// state them.
type commandCodeConfig struct {
	model, effort string
	efforts       []string
}

func parseCommandCodeConfig(raw json.RawMessage) (commandCodeConfig, bool) {
	var options []struct {
		ID      string          `json:"id"`
		Current json.RawMessage `json:"currentValue"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if json.Unmarshal(raw, &options) != nil {
		return commandCodeConfig{}, false
	}
	var c commandCodeConfig
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
		}
	}
	return c, c.model != ""
}

// commandCodeSessionState is what session/new or session/resume reported.
type commandCodeSessionState struct {
	id, mode string
	modes    []string
	config   commandCodeConfig
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
	config, ok := parseCommandCodeConfig(r.Config)
	if !ok {
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
	body, err := s.transport.request(ctx, "initialize", map[string]any{"protocolVersion": commandCodeProtocolVersion, "clientCapabilities": map[string]any{}})
	if err != nil {
		return err
	}
	agent, err := parseCommandCodeAgent(body)
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
			s.closeCommandCodeSession(id)
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
	s.mu.Lock()
	s.commandCodeWatch, s.commandCodeMode = id, state.mode
	s.mu.Unlock()
	if state.mode != commandCodeAskingMode {
		if !slices.Contains(state.modes, commandCodeAskingMode) {
			return &CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedPermissionMode, Phase: BeforeFirstPrompt}
		}
		if _, err := s.transport.request(ctx, "session/set_mode", map[string]any{"sessionId": id, "modeId": commandCodeAskingMode}); err != nil {
			return commandCodeConfigRefused(err, CapabilityChangedPermissionMode)
		}
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
	s.commandCodeModel, s.commandCodeConfigured = config.model, true
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
	}
	stop := ours && s.commandCodeConfigured && p.Update.Mode != commandCodeAskingMode
	s.mu.Unlock()
	if stop {
		s.fail(&CapabilityError{Engine: harness.CommandCode, Code: CapabilityChangedPermissionMode, Phase: DuringSession})
	}
	return true
}

func (s *Session) setCommandCodeOption(ctx context.Context, id, option, value, code string) (commandCodeConfig, error) {
	body, err := s.transport.request(ctx, "session/set_config_option", map[string]any{"sessionId": id, "configId": option, "value": value})
	if err != nil {
		return commandCodeConfig{}, commandCodeConfigRefused(err, code)
	}
	var r struct {
		Config json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(body, &r) != nil {
		return commandCodeConfig{}, ErrProtocol
	}
	config, ok := parseCommandCodeConfig(r.Config)
	if !ok {
		return commandCodeConfig{}, ErrProtocol
	}
	return config, nil
}

// commandCodeConfigRefused turns Command Code's refusal of a setting into the
// capability failure it means. Anything else, such as a lost transport, is
// returned as it is.
func commandCodeConfigRefused(err error, code string) error {
	var refusal *commandCodeRefusal
	if errors.As(err, &refusal) && (refusal.code == commandCodeInvalidParams || refusal.code == commandCodeMethodMissing) {
		return &CapabilityError{Engine: harness.CommandCode, Code: code, Phase: BeforeFirstPrompt}
	}
	return err
}

// closeCommandCodeSession closes a session this launch created and then
// refused before any prompt. Command Code lists only conversations that have
// a message, so nothing of it remains. It is best effort: the session is
// closing.
func (s *Session) closeCommandCodeSession(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.transport.request(ctx, "session/close", map[string]any{"sessionId": id})
}

// commandCodeServerReply answers a request from the agent. Only a permission
// request is answered, by the session's policy and never with an "always"
// option, which would grant or refuse the tool for the rest of the session.
// Anything else is refused with a JSON-RPC error rather than left unanswered.
func commandCodeServerReply(m map[string]json.RawMessage, permission string) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m["id"]}
	if str(m, "method") != "session/request_permission" {
		reply["error"] = map[string]any{"code": -32601, "message": "Client does not authorize this operation"}
		return reply
	}
	reply["result"] = map[string]any{"outcome": commandCodePermissionOutcome(m["params"], permission)}
	return reply
}

// commandCodePermissionOutcome selects the allow-once option under
// CommandCodeAllowWhenAsked and the reject-once option otherwise, and only
// when the request offers both. A tool that would change the permission mode
// is rejected under either policy: allowing it would let every later change
// run without asking. Command Code asks the user a question through the same
// method, offering only allow-once options, one per answer; such a request is
// answered cancelled, as is anything unreadable, so no answer and no
// permission is ever guessed.
func commandCodePermissionOutcome(raw json.RawMessage, permission string) map[string]any {
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
	for _, option := range p.Options {
		if option.ID == "" {
			continue
		}
		if _, seen := chosen[option.Kind]; seen {
			return cancelled
		}
		chosen[option.Kind] = option.ID
	}
	allow, reject := chosen["allow_once"], chosen["reject_once"]
	if allow == "" || reject == "" {
		return cancelled
	}
	if permission == CommandCodeAllowWhenAsked && p.ToolCall.Kind != "switch_mode" {
		return map[string]any{"outcome": "selected", "optionId": allow}
	}
	return map[string]any{"outcome": "selected", "optionId": reject}
}

// commandCodeRefusal is a JSON-RPC error from Command Code, classified into a
// fixed code. The agent's own message and data are read only to classify it
// and are never kept.
type commandCodeRefusal struct {
	rpc  int
	code string
}

// Command Code refusal codes.
const (
	commandCodeMethodMissing       = "method_not_found"
	commandCodeInvalidParams       = "invalid_params"
	commandCodeBusy                = "prompt_running"
	commandCodeAuthRequired        = "authentication_required"
	commandCodeAuthFailed          = "authentication_failed"
	commandCodeRateLimited         = "rate_limited"
	commandCodeTimedOut            = "timeout"
	commandCodeProviderUnavailable = "provider_unavailable"
	commandCodeProviderRejected    = "provider_rejected"
	commandCodeRejected            = "rejected"
)

func (e *commandCodeRefusal) Error() string { return "command code refused the request: " + e.code }

func (e *commandCodeRefusal) Unwrap() error {
	if e.rpc == -32601 {
		return ErrUnsupported
	}
	return ErrRejected
}

func (e *commandCodeRefusal) HarnessFacts() harness.Facts {
	facts := harness.Facts{Engine: harness.CommandCode, Operation: harness.Session, Family: harness.FailureRequest, Cause: harness.CauseUnknown, Code: e.code}
	switch e.code {
	case commandCodeMethodMissing:
		facts.Family, facts.Cause = harness.FailureCapability, ""
	case commandCodeAuthRequired, commandCodeAuthFailed:
		facts.Cause = harness.CauseAuthentication
	case commandCodeRateLimited:
		facts.Cause = harness.CauseRateLimited
	case commandCodeTimedOut:
		facts.Cause = harness.CauseTimeout
	case commandCodeProviderUnavailable:
		facts.Cause = harness.CauseUnavailable
	}
	return facts
}

// parseCommandCodeReply reads a JSON-RPC response as Codex's does, then
// classifies an error reply from what Command Code states in fixed fields.
func parseCommandCodeReply(m map[string]json.RawMessage) (string, response, bool, error) {
	id, r, isReply, err := parseCodexReply(m)
	if !isReply || err != nil || r.err == nil {
		return id, r, isReply, err
	}
	var e struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(m["error"], &e) != nil {
		return "", response{}, true, ErrProtocol
	}
	return id, response{err: &commandCodeRefusal{rpc: e.Code, code: commandCodeRefusalCode(e.Code, e.Data)}}, true, nil
}

// commandCodeRefusalCode classifies by the JSON-RPC code and, for a failed
// run, the provider's HTTP status that Command Code passes on in data.
func commandCodeRefusalCode(code int, data json.RawMessage) string {
	switch code {
	case -32601:
		return commandCodeMethodMissing
	case -32602:
		return commandCodeInvalidParams
	case -32600:
		return commandCodeBusy
	case -32000:
		return commandCodeAuthRequired
	}
	var d struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal(data, &d)
	switch {
	case d.Status == 401 || d.Status == 403:
		return commandCodeAuthFailed
	case d.Status == 429:
		return commandCodeRateLimited
	case d.Status == 408 || d.Status == 504:
		return commandCodeTimedOut
	case d.Status >= 500:
		return commandCodeProviderUnavailable
	case d.Status >= 400:
		return commandCodeProviderRejected
	}
	return commandCodeRejected
}
