package session

// Grok's Agent Client Protocol: JSON-RPC 2.0 over the stdio of
// `grok agent --no-leader stdio`. Every method and shape here was checked
// against grok 1.0.41:
//
//   - initialize {protocolVersion: 1, clientCapabilities: {}} answers with
//     protocolVersion 1 and agentCapabilities, whose sessionCapabilities
//     advertise resume.
//   - session/new {cwd, mcpServers: []} answers with the sessionId, the model
//     state and the session's configOptions. It runs the operator's configured
//     SessionStart hooks, as an ordinary native session does. session/resume
//     takes the same fields and the sessionId; a conversation Grok does not
//     have is refused with data.code FS_NOT_FOUND.
//   - _meta.rules is appended to the system prompt and _meta.systemPromptOverride
//     replaces it. Both must be sent again on resume, or the resumed session
//     runs without them.
//   - session/prompt streams session/update notifications and answers, when
//     the turn is over, with its stopReason and the turn's own usage in _meta.
//   - session/cancel is a notification; the running prompt then answers with
//     stopReason "cancelled". A second prompt sent while one runs is queued
//     behind it, which this library never does.
//   - session/request_permission is a request from the agent. Grok asks only
//     where a permission rule or its mode says to; otherwise, in agent mode, it
//     runs edits and shell commands without asking.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// grokProtocolVersion is the only Agent Client Protocol version spoken here.
const grokProtocolVersion = 1

func grokInitializeParams() map[string]any {
	return map[string]any{"protocolVersion": grokProtocolVersion, "clientCapabilities": map[string]any{}}
}

// grokSessionParams creates, or resumes by id, a session in the working
// directory with the session's instructions. No MCP server is added: the
// operator's own configuration is Grok's to load.
func grokSessionParams(o Options, resume bool, id string) map[string]any {
	p := map[string]any{"cwd": o.WorkDir, "mcpServers": []any{}}
	switch instructions := effectiveInstructions(o); instructions.Mode {
	case Append:
		p["_meta"] = map[string]any{"rules": instructions.Text}
	case Replace:
		p["_meta"] = map[string]any{"systemPromptOverride": instructions.Text}
	}
	if resume {
		p["sessionId"] = id
	}
	return p
}

func grokPromptParams(session, text string) map[string]any {
	return map[string]any{"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": text}}}
}

func grokNotification(method string, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
}

// grokAgent is what initialize established about the installed agent.
type grokAgent struct{ resume bool }

func parseGrokAgent(raw json.RawMessage) (grokAgent, error) {
	var r struct {
		ProtocolVersion *int `json:"protocolVersion"`
		Capabilities    struct {
			Session struct {
				Resume json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ProtocolVersion == nil || *r.ProtocolVersion != grokProtocolVersion {
		return grokAgent{}, ErrProtocol
	}
	resume := len(r.Capabilities.Session.Resume) != 0 && string(r.Capabilities.Session.Resume) != "null"
	return grokAgent{resume: resume}, nil
}

// grokSessionState is what session/new or session/resume reported about the
// session it opened.
type grokSessionState struct {
	id, model, effort string
	effortOffered     bool
	capacity          int64
}

func parseGrokSession(raw json.RawMessage) (grokSessionState, error) {
	var r struct {
		SessionID string `json:"sessionId"`
		Models    *struct {
			Current   string `json:"currentModelId"`
			Available []struct {
				ID   string `json:"modelId"`
				Meta struct {
					Context int64 `json:"totalContextTokens"`
				} `json:"_meta"`
			} `json:"availableModels"`
		} `json:"models"`
		Config []struct {
			ID      string          `json:"id"`
			Current json.RawMessage `json:"currentValue"`
		} `json:"configOptions"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Models == nil {
		return grokSessionState{}, ErrProtocol
	}
	state := grokSessionState{id: r.SessionID, model: r.Models.Current}
	for _, m := range r.Models.Available {
		if m.ID == state.model && m.Meta.Context > 0 {
			state.capacity = m.Meta.Context
		}
	}
	for _, option := range r.Config {
		if option.ID == "reasoning_effort" {
			state.effortOffered = json.Unmarshal(option.Current, &state.effort) == nil
		}
	}
	return state, nil
}

// grokConfigMismatch names what the opened session does not honour. Grok
// substitutes another model for one the login may not use, and drops an effort
// the model does not offer, both without failing, so the session's own report
// is checked rather than the flags assumed.
func grokConfigMismatch(o Options, state grokSessionState) string {
	if o.Model != "" && state.model != o.Model {
		return CapabilityChangedModel
	}
	if o.Effort != "" && (!state.effortOffered || state.effort != o.Effort) {
		return CapabilityChangedEffort
	}
	return ""
}

// initializeGrok opens the protocol and creates or resumes the session.
func (s *Session) initializeGrok(ctx context.Context, resume bool) error {
	body, err := s.transport.request(ctx, "initialize", grokInitializeParams())
	if err != nil {
		return err
	}
	agent, err := parseGrokAgent(body)
	if err != nil {
		return err
	}
	if resume && !agent.resume {
		return &UnsupportedError{Engine: harness.Grok, Operation: "resume", Code: RefusedMethodMissing, Capability: harness.Capability{Availability: harness.Unsupported, Reason: "the installed Grok does not offer session/resume"}}
	}
	method := "session/new"
	if resume {
		method = "session/resume"
	}
	s.mu.Lock()
	id := s.ref.ID
	s.mu.Unlock()
	body, err = s.transport.request(ctx, method, grokSessionParams(s.options, resume, id))
	if err != nil {
		if resume && grokSessionGone(err) {
			return fmt.Errorf("%w: %w", errConversationGone, err)
		}
		return err
	}
	state, err := parseGrokSession(body)
	if err != nil {
		return err
	}
	if !resume {
		if state.id == "" {
			return ErrProtocol
		}
		id = state.id
	}
	if code := grokConfigMismatch(s.options, state); code != "" {
		if !resume {
			s.discardGrokSession(id)
		}
		return &CapabilityError{Engine: harness.Grok, Code: code, Phase: BeforeFirstPrompt}
	}
	s.mu.Lock()
	s.ref.ID = id
	s.grokModel, s.grokCapacity = state.model, state.capacity
	s.mu.Unlock()
	return nil
}

// discardGrokSession removes a session this launch created and then refused
// before any prompt, so a refused configuration leaves no empty conversation in
// the operator's Grok history. It is best effort: the session is closing.
func (s *Session) discardGrokSession(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.transport.request(ctx, "_x.ai/session/delete", map[string]any{"sessionId": id})
}

// grokServerReply answers a request from the agent. A permission request is
// answered by the session's policy and never with an "always" option, which
// would persist a grant or a rule in the operator's configuration. Anything
// else is refused with a JSON-RPC error rather than left unanswered.
func grokServerReply(m map[string]json.RawMessage, permission string) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m["id"]}
	if str(m, "method") != "session/request_permission" {
		reply["error"] = map[string]any{"code": -32601, "message": "Client does not authorize this operation"}
		return reply
	}
	reply["result"] = map[string]any{"outcome": grokPermissionOutcome(m["params"], permission)}
	return reply
}

// grokPermissionOutcome selects the allow-once option under GrokAllowWhenAsked and the
// reject-once option otherwise. Without the option it wants, it answers
// cancelled, which Grok treats as a refusal: an allow is never guessed.
func grokPermissionOutcome(raw json.RawMessage, permission string) map[string]any {
	want := "reject_once"
	if permission == GrokAllowWhenAsked {
		want = "allow_once"
	}
	var p struct {
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(raw, &p) == nil {
		for _, option := range p.Options {
			if option.Kind == want && option.ID != "" {
				return map[string]any{"outcome": "selected", "optionId": option.ID}
			}
		}
	}
	return map[string]any{"outcome": "cancelled"}
}

// grokRefusal is a JSON-RPC error from Grok, classified into a fixed code.
// The agent's own message and data are read only to classify it and are never
// kept.
type grokRefusal struct {
	rpc  int
	code string
}

// Grok refusal codes.
const (
	grokMethodMissing       = "method_not_found"
	grokSessionNotFound     = "session_not_found"
	grokAuthRequired        = "authentication_required"
	grokAuthFailed          = "authentication_failed"
	grokRateLimited         = "rate_limited"
	grokTimedOut            = "timeout"
	grokProviderUnavailable = "provider_unavailable"
	grokProviderRejected    = "provider_rejected"
	grokRejected            = "rejected"
)

func (e *grokRefusal) Error() string { return "grok refused the request: " + e.code }

func (e *grokRefusal) Unwrap() error {
	if e.rpc == -32601 {
		return ErrUnsupported
	}
	return ErrRejected
}

func (e *grokRefusal) HarnessFacts() harness.Facts {
	facts := harness.Facts{Engine: harness.Grok, Operation: harness.Session, Family: harness.FailureRequest, Cause: harness.CauseUnknown, Code: e.code}
	switch e.code {
	case grokMethodMissing:
		facts.Family, facts.Cause = harness.FailureCapability, ""
	case grokAuthRequired, grokAuthFailed:
		facts.Cause = harness.CauseAuthentication
	case grokRateLimited:
		facts.Cause = harness.CauseRateLimited
	case grokTimedOut:
		facts.Cause = harness.CauseTimeout
	case grokProviderUnavailable:
		facts.Cause = harness.CauseUnavailable
	}
	return facts
}

// parseGrokReply reads a JSON-RPC response as Codex's does, then classifies
// an error reply from what Grok states in fixed fields.
func parseGrokReply(m map[string]json.RawMessage) (string, response, bool, error) {
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
	return id, response{err: &grokRefusal{rpc: e.Code, code: grokRefusalCode(e.Code, e.Message, e.Data)}}, true, nil
}

func grokRefusalCode(code int, message string, data json.RawMessage) string {
	if code == -32601 {
		return grokMethodMissing
	}
	var d struct {
		Code   string `json:"code"`
		Status int    `json:"http_status"`
	}
	_ = json.Unmarshal(data, &d)
	switch {
	case d.Code == "FS_NOT_FOUND":
		return grokSessionNotFound
	case code == -32000 && strings.Contains(message, "Authentication required"):
		return grokAuthRequired
	case d.Status == 401 || d.Status == 403:
		return grokAuthFailed
	case d.Status == 429:
		return grokRateLimited
	case d.Status == 408 || d.Status == 504:
		return grokTimedOut
	case d.Status >= 500:
		return grokProviderUnavailable
	case d.Status >= 400:
		return grokProviderRejected
	}
	return grokRejected
}

func grokSessionGone(err error) bool {
	var refusal *grokRefusal
	return errors.As(err, &refusal) && refusal.code == grokSessionNotFound
}

// grokAccountMethod reads the login on the session's own agent process. It
// creates no session and sends nothing to a model.
const grokAccountMethod = "_x.ai/auth/check_subscription"

// parseGrokAccount reads a check_subscription answer, as package account does
// for an inspection. Nil LoggedIn and empty fields stay unknown.
func parseGrokAccount(raw json.RawMessage) (harness.AccountSnapshot, error) {
	var r struct {
		Authenticated *bool `json:"authenticated"`
		Meta          *struct {
			Email    string  `json:"email"`
			AuthMode string  `json:"auth_mode"`
			Tier     string  `json:"subscription_tier"`
			Team     *string `json:"team_name"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Authenticated == nil {
		return harness.AccountSnapshot{}, ErrProtocol
	}
	a := harness.AccountSnapshot{Observation: observation(grokAccountMethod, harness.Measured), LoggedIn: r.Authenticated}
	if !*r.Authenticated || r.Meta == nil {
		return a, nil
	}
	a.Email, a.AuthMethod, a.Plan = r.Meta.Email, r.Meta.AuthMode, r.Meta.Tier
	if r.Meta.Team != nil {
		a.Organization = *r.Meta.Team
	}
	return a, nil
}
