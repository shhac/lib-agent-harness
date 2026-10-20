package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

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

// grokProtocolVersion is the only Agent Client Protocol version spoken here.
const grokProtocolVersion = 1

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

func grokNotification(method string, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
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
	body, err := s.transport.request(ctx, "initialize", acpInitializeParams(grokProtocolVersion))
	if err != nil {
		return err
	}
	agent, err := parseACPAgent(body, grokProtocolVersion)
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
		if resume && acpSessionGone(err) {
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
	s.acpModel, s.grokCapacity = state.model, state.capacity
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

// grokDialect is Grok's ACP, answering permission requests by the session's
// policy and never with an "always" option, which would persist a grant or a
// rule in the operator's configuration.
func grokDialect(p Policy) dialect {
	return acpDialect(harness.Grok, grokRefusalCode, func(params json.RawMessage) map[string]any {
		return grokPermissionOutcome(params, p.GrokPermission)
	})
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

// acpSessionNotFound is Grok's refusal of a conversation it does not hold.

func grokRefusalCode(code int, message string, data json.RawMessage) string {
	if code == -32601 {
		return acpMethodMissing
	}
	var d struct {
		Code   string `json:"code"`
		Status int    `json:"http_status"`
	}
	_ = json.Unmarshal(data, &d)
	if d.Code == "FS_NOT_FOUND" {
		return acpSessionNotFound
	}
	if code == -32000 && strings.Contains(message, "Authentication required") {
		return acpAuthRequired
	}
	if status := acpStatusCode(d.Status); status != "" {
		return status
	}
	return acpRejected
}

// grokAccountMethod reads the login on the session's own agent process. It
// creates no session and sends nothing to a model.
const grokAccountMethod = "_x.ai/auth/check_subscription"

// parseGrokAccount reads a check_subscription answer, as package account does
// for an inspection.
func parseGrokAccount(raw json.RawMessage) (harness.AccountSnapshot, error) {
	a, ok := nativecli.GrokSubscription(raw, observation(grokAccountMethod, harness.Measured))
	if !ok {
		return harness.AccountSnapshot{}, ErrProtocol
	}
	return a, nil
}

func normalizeGrokPolicy(o Options) (Options, error) {
	switch o.Policy.GrokPermission {
	case "":
		return o, refuse(o, "policy", RefusedPolicy, "Grok's agent mode runs edits and commands without asking; set Policy.GrokPermission to GrokDenyWhenAsked or GrokAllowWhenAsked knowingly")
	case GrokDenyWhenAsked, GrokAllowWhenAsked:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Grok permission policy")
	}
	switch o.Policy.GrokTelemetry {
	case "", GrokTelemetryReduced:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Grok telemetry policy")
	}
	return o, nil
}

// grokManaged is what a Grok session never inherits: its home, which the
// session selects, and the credentials and endpoint overrides that would move
// it off the selected login.
func grokManaged(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "XAI_") || strings.HasPrefix(upper, "GROK_AUTH") {
		return true
	}
	if !strings.HasPrefix(upper, "GROK_") {
		return false
	}
	return upper == "GROK_HOME" || upper == "GROK_DEPLOYMENT_KEY" || upper == "GROK_AGENT_SECRET" || strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_URL")
}

// refuseGrokAddition refuses every GROK_ and XAI_ addition, more than a Grok
// session withholds on inheritance: the operator's own Grok settings carry
// over, but a caller sets Grok's controls only through Policy.
func refuseGrokAddition(o Options, key, upper string) error {
	if strings.HasPrefix(upper, "GROK_") || strings.HasPrefix(upper, "XAI_") {
		return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" is managed by the harness; set Policy.GrokTelemetry for Grok's telemetry controls")
	}
	return nil
}

func grokOverrides(o Options, env []string) []string {
	if o.Policy.GrokTelemetry != GrokTelemetryReduced {
		return env
	}
	return nativecli.Override(env, nativecli.GrokReducedTelemetry...)
}

// grokArgs starts Grok's agent protocol on stdio, always as its own process:
// a shared leader would carry another client's sessions and configuration.
// Values are bound with "=", because Grok reads a separate value beginning
// with "-" as a missing one.
func grokArgs(o Options) []string {
	args := []string{"agent", "--no-leader"}
	if o.Model != "" {
		args = append(args, "--model="+o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--reasoning-effort="+o.Effort)
	}
	if o.skills != nil && o.skills.delivery == nativeSkills {
		args = append(args, "--plugin-dir="+o.skills.pluginDir)
	}
	return append(args, "stdio")
}
